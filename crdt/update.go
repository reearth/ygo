package crdt

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/reearth/ygo/encoding"
)

// Wire content type tags matching the Yjs V1 protocol.
const (
	wireDeleted byte = 1
	wireJSON    byte = 2
	wireBinary  byte = 3
	wireString  byte = 4
	wireEmbed   byte = 5
	wireFormat  byte = 6
	wireType    byte = 7
	wireAny     byte = 8
	wireDoc     byte = 9
	// Tag 10 is reserved for skip structs (decode-only, handled before the switch).
	wireMove byte = 11 // ContentMove: CRDT-safe array move marker (ygo extension)
)

// Info byte flags for struct encoding.
const (
	flagHasOrigin      byte = 0x80
	flagHasRightOrigin byte = 0x40
	flagHasParentSub   byte = 0x20
)

// ErrInvalidUpdate is returned when a binary update cannot be decoded.
var ErrInvalidUpdate = errors.New("crdt: invalid update")

// ── Public API ────────────────────────────────────────────────────────────────

// EncodeStateAsUpdateV1 encodes the part of doc newer than sv as a V1 binary
// update. Pass nil to encode the entire document state.
func EncodeStateAsUpdateV1(doc *Doc, sv StateVector) []byte {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	return encodeV1Locked(doc, sv)
}

// ApplyUpdateV1 decodes and integrates a V1 binary update into doc.
func ApplyUpdateV1(doc *Doc, update []byte, origin any) error {
	var applyErr error
	doc.Transact(func(txn *Transaction) {
		applyErr = applyV1Txn(txn, update)
	}, origin)
	return applyErr
}

// EncodeStateAsUpdateV2 encodes the document state using the Yjs V2
// column-oriented binary format.  The output is interoperable with
// Y.applyUpdateV2 / Y.encodeStateAsUpdateV2 from the yjs npm package.
func EncodeStateAsUpdateV2(doc *Doc, sv StateVector) []byte {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	return encodeV2Locked(doc, sv)
}

// ApplyUpdateV2 decodes and integrates a Yjs V2 binary update into doc.
func ApplyUpdateV2(doc *Doc, update []byte, origin any) error {
	var applyErr error
	doc.Transact(func(txn *Transaction) {
		applyErr = applyV2Txn(txn, update)
	}, origin)
	return applyErr
}

// UpdateV1ToV2 converts a V1 update payload to real Yjs V2 format by applying
// it to a temporary document and re-encoding in V2.
func UpdateV1ToV2(v1 []byte) ([]byte, error) {
	doc := New()
	if err := ApplyUpdateV1(doc, v1, nil); err != nil {
		return nil, err
	}
	return EncodeStateAsUpdateV2(doc, nil), nil
}

// UpdateV2ToV1 converts a real Yjs V2 update to V1 format by applying it to a
// temporary document and re-encoding in V1.
func UpdateV2ToV1(v2 []byte) ([]byte, error) {
	doc := New()
	if err := ApplyUpdateV2(doc, v2, nil); err != nil {
		return nil, err
	}
	return EncodeStateAsUpdateV1(doc, nil), nil
}

// MergeUpdatesV1 combines multiple V1 updates into one by applying them all
// to a temporary document and re-encoding its full state.
func MergeUpdatesV1(updates ...[]byte) ([]byte, error) {
	doc := New()
	for _, u := range updates {
		if err := ApplyUpdateV1(doc, u, nil); err != nil {
			return nil, err
		}
	}
	return EncodeStateAsUpdateV1(doc, nil), nil
}

// DiffUpdateV1 returns the subset of update that is missing from sv.
func DiffUpdateV1(update []byte, sv StateVector) ([]byte, error) {
	doc := New()
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		return nil, err
	}
	return EncodeStateAsUpdateV1(doc, sv), nil
}

// EncodeStateVectorV1 serialises the document's state vector as a compact
// binary blob (VarUint count, then client/clock pairs).
func EncodeStateVectorV1(doc *Doc) []byte {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	sv := doc.store.StateVector()
	enc := encoding.NewEncoder()
	clients := clientsSorted(sv)
	enc.WriteVarUint(uint64(len(clients)))
	for _, c := range clients {
		enc.WriteVarUint(uint64(c))
		enc.WriteVarUint(sv[c])
	}
	return enc.Bytes()
}

// DecodeStateVectorV1 parses a blob produced by EncodeStateVectorV1.
func DecodeStateVectorV1(data []byte) (StateVector, error) {
	dec := encoding.NewDecoder(data)
	n, err := dec.ReadVarUint()
	if err != nil {
		return nil, wrapUpdateErr(err)
	}
	// Each entry requires at least 2 bytes (client varuint + clock varuint).
	// Guard against a crafted count that would cause a multi-GB map allocation.
	if n > uint64(len(data)/2) || n > maxV2Items {
		return nil, ErrInvalidUpdate
	}
	sv := make(StateVector, n)
	for i := uint64(0); i < n; i++ {
		c, err := dec.ReadVarUint()
		if err != nil {
			return nil, wrapUpdateErr(err)
		}
		clock, err := dec.ReadVarUint()
		if err != nil {
			return nil, wrapUpdateErr(err)
		}
		sv[ClientID(c)] = clock
	}
	return sv, nil
}

// ── V1 encoding ───────────────────────────────────────────────────────────────

func encodeV1Locked(doc *Doc, sv StateVector) []byte {
	enc := encoding.NewEncoder()

	type clientGroup struct {
		client     ClientID
		items      []*Item
		startClock uint64
	}

	var groups []clientGroup
	for client, items := range doc.store.clients {
		svClock := sv.Clock(client)
		var relevant []*Item
		for _, item := range items {
			if item.ID.Clock+uint64(item.Content.Len()) > svClock {
				relevant = append(relevant, item)
			}
		}
		if len(relevant) > 0 {
			groups = append(groups, clientGroup{client, relevant, svClock})
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].client < groups[j].client })

	enc.WriteVarUint(uint64(len(groups)))
	for _, g := range groups {
		enc.WriteVarUint(uint64(len(g.items)))
		enc.WriteVarUint(uint64(g.client))
		enc.WriteVarUint(g.startClock)
		for i, item := range g.items {
			offset := 0
			if i == 0 && g.startClock > item.ID.Clock {
				offset = int(g.startClock - item.ID.Clock)
			}
			encodeItem(enc, item, offset, doc.store)
		}
	}

	encodeDeleteSet(enc, buildDeleteSet(doc.store))
	return enc.Bytes()
}

func encodeItem(enc *encoding.Encoder, item *Item, offset int, store *StructStore) {
	// Orphaned items (no parent) came from GC wire format where the parent
	// type name is lost. Encode them as GC structs so receivers get valid
	// clock accounting instead of corrupt data.
	if item.Parent == nil {
		length := item.Content.Len()
		if offset > 0 {
			length -= offset
		}
		enc.WriteUint8(0) // GC struct info byte
		enc.WriteVarUint(uint64(length))
		return
	}

	var tag byte
	switch item.Content.(type) {
	case *ContentDeleted:
		tag = wireDeleted
	case *ContentJSON:
		tag = wireJSON
	case *ContentBinary:
		tag = wireBinary
	case *ContentString:
		tag = wireString
	case *ContentEmbed:
		tag = wireEmbed
	case *ContentFormat:
		tag = wireFormat
	case *ContentType:
		tag = wireType
	case *ContentAny:
		tag = wireAny
	case *ContentDoc:
		tag = wireDoc
	case *ContentMove:
		tag = wireMove
	default:
		tag = wireAny
	}

	// Effective origins for this encoded slice.
	var origin, originRight *ID
	if offset > 0 {
		oc := item.ID.Clock + uint64(offset) - 1
		origin = &ID{Client: item.ID.Client, Clock: oc}
		originRight = item.OriginRight
	} else {
		origin = item.Origin
		originRight = item.OriginRight
	}

	// If the origin item is a GC placeholder (no Parent), the receiver can't
	// infer this item's parent from it. Clear the origin so that explicit
	// parent info is encoded instead, allowing the receiver to resolve the
	// parent directly from the named root type or container item ID.
	if origin != nil {
		if oi := store.Find(*origin); oi != nil && oi.Parent == nil {
			origin = nil
		}
	}
	if originRight != nil {
		if ori := store.Find(*originRight); ori != nil && ori.Parent == nil {
			originRight = nil
		}
	}

	info := tag
	if origin != nil {
		info |= flagHasOrigin
	}
	if originRight != nil {
		info |= flagHasRightOrigin
	}
	if item.ParentSub != "" {
		info |= flagHasParentSub
	}
	enc.WriteUint8(info)

	if origin != nil {
		enc.WriteVarUint(uint64(origin.Client))
		enc.WriteVarUint(origin.Clock)
	}
	if originRight != nil {
		enc.WriteVarUint(uint64(originRight.Client))
		enc.WriteVarUint(originRight.Clock)
	}

	// Parent info — only when neither origin is present.
	if origin == nil && originRight == nil {
		if item.Parent != nil && item.Parent.item != nil {
			// Nested type: identify by container item's ID.
			enc.WriteUint8(0)
			enc.WriteVarUint(uint64(item.Parent.item.ID.Client))
			enc.WriteVarUint(item.Parent.item.ID.Clock)
		} else {
			// Root named type.
			enc.WriteUint8(1)
			name := ""
			if item.Parent != nil {
				name = item.Parent.name
			}
			enc.WriteVarString(name)
		}
	}

	if item.ParentSub != "" {
		enc.WriteVarString(item.ParentSub)
	}

	encodeContent(enc, item.Content, offset)
}

func encodeContent(enc *encoding.Encoder, c Content, offset int) {
	switch ct := c.(type) {
	case *ContentDeleted:
		enc.WriteVarUint(uint64(ct.length - offset))
	case *ContentJSON:
		vals := ct.Vals[offset:]
		enc.WriteVarUint(uint64(len(vals)))
		for _, v := range vals {
			enc.WriteAny(v)
		}
	case *ContentBinary:
		enc.WriteVarBytes(ct.Data)
	case *ContentString:
		byteOff := utf16ByteOffset(ct.Str, offset)
		enc.WriteVarString(ct.Str[byteOff:])
	case *ContentEmbed:
		enc.WriteAny(ct.Val)
	case *ContentFormat:
		enc.WriteVarString(ct.Key)
		enc.WriteVarString(fmtValToJSON(ct.Val))
	case *ContentType:
		tc, nodeName := typeClassOf(ct)
		enc.WriteUint8(tc)
		if tc == 3 { // YXmlElement
			enc.WriteVarString(nodeName)
		}
	case *ContentAny:
		vals := ct.Vals[offset:]
		enc.WriteVarUint(uint64(len(vals)))
		for _, v := range vals {
			enc.WriteAny(v)
		}
	case *ContentDoc:
		guid := ""
		if ct.Doc != nil {
			guid = ct.Doc.GUID()
		}
		enc.WriteVarBytes([]byte(guid))
	case *ContentMove:
		enc.WriteVarUint(uint64(ct.Target.Client))
		enc.WriteVarUint(ct.Target.Clock)
		enc.WriteVarUint(uint64(ct.TargetLen))
	}
}

func typeClassOf(ct *ContentType) (byte, string) {
	if ct.Type == nil || ct.Type.owner == nil {
		return 0, ""
	}
	switch v := ct.Type.owner.(type) {
	case *YArray:
		return 0, ""
	case *YMap:
		return 1, ""
	case *YText:
		return 2, ""
	case *YXmlElement:
		return 3, v.NodeName
	case *YXmlFragment:
		return 4, ""
	case *YXmlText:
		return 6, ""
	default:
		return 0, ""
	}
}

func buildDeleteSet(store *StructStore) DeleteSet {
	ds := newDeleteSet()
	for _, items := range store.clients {
		for _, item := range items {
			if item.Deleted {
				ds.add(item.ID, item.Content.Len())
			}
		}
	}
	for client := range ds.clients {
		ds.sortAndCompact(client)
	}
	return ds
}

func encodeDeleteSet(enc *encoding.Encoder, ds DeleteSet) {
	clients := make([]ClientID, 0, len(ds.clients))
	for c := range ds.clients {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	enc.WriteVarUint(uint64(len(clients)))
	for _, c := range clients {
		ranges := ds.clients[c]
		enc.WriteVarUint(uint64(c))
		enc.WriteVarUint(uint64(len(ranges)))
		for _, r := range ranges {
			enc.WriteVarUint(r.Clock)
			enc.WriteVarUint(r.Len)
		}
	}
}

// ── V1 decoding ───────────────────────────────────────────────────────────────

func applyV1Txn(txn *Transaction, update []byte) (retErr error) {
	// Recover from panics emitted by Content.Splice on non-splittable types
	// (ContentBinary, ContentEmbed, ContentFormat, ContentType, ContentDoc).
	// A malicious update can encode such a type with a clock offset that forces
	// a split; without recovery the server would crash instead of returning an error.
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("%w: panic during item integration: %v", ErrInvalidUpdate, r)
		}
	}()

	dec := encoding.NewDecoder(update)

	// Snapshot state vector before applying anything (used for skip/offset logic).
	sv := txn.doc.store.StateVector()

	numClients, err := dec.ReadVarUint()
	if err != nil {
		return wrapUpdateErr(err)
	}
	if numClients > uint64(len(update)/2) || numClients > maxV2Items {
		return ErrInvalidUpdate
	}

	var pending []*Item

	totalStructs := uint64(0)
	for i := uint64(0); i < numClients; i++ {
		numStructs, err := dec.ReadVarUint()
		if err != nil {
			return wrapUpdateErr(err)
		}
		totalStructs += numStructs
		if totalStructs > maxV2Items {
			return ErrInvalidUpdate
		}
		clientU, err := dec.ReadVarUint()
		if err != nil {
			return wrapUpdateErr(err)
		}
		client := ClientID(clientU)
		clock, err := dec.ReadVarUint()
		if err != nil {
			return wrapUpdateErr(err)
		}

		existingEnd := sv.Clock(client)

		for j := uint64(0); j < numStructs; j++ {
			item, err := decodeItem(dec, txn.doc, client, clock)
			if err != nil {
				return wrapUpdateErr(err)
			}

			// Skip structs (tag 10) are clock-range placeholders that are
			// never stored — just advance the clock. Update existingEnd so
			// that subsequent items in this group are not mistakenly flagged
			// as having a clock gap (skip structs tell the receiver those
			// clocks are intentionally absent).
			if _, isSkip := item.Content.(*contentSkip); isSkip {
				skipEnd := clock + uint64(item.Content.Len())
				if skipEnd > existingEnd {
					existingEnd = skipEnd
				}
				clock = skipEnd
				continue
			}

			contentLen := uint64(item.Content.Len())
			itemEnd := clock + contentLen

			if itemEnd <= existingEnd {
				// Already fully integrated — skip.
				clock = itemEnd
				continue
			}

			offset := 0
			if clock < existingEnd {
				// Partially integrated — integrate only the new suffix.
				offset = int(existingEnd - clock)
			}

			// Same-client clock gap: this item's clock is past the store's
			// current clock for this client (we have 0..existingEnd but this
			// item starts at clock > existingEnd). Silently integrating would
			// misplace the item at the head of its parent list. Park instead,
			// with the store's current clock as the watermark — when the store
			// reaches that clock, the missing predecessor may be available.
			if clock > existingEnd {
				if txn.doc.store.pending == nil {
					txn.doc.store.pending = &pendingUpdate{missing: make(StateVector)}
				}
				txn.doc.store.pending.items = append(txn.doc.store.pending.items, item)
				mergePendingMissing(txn.doc.store.pending.missing, client, existingEnd)
				clock = itemEnd
				continue
			}

			// GC items (tag 0) have no parent — add directly to the store
			// without linked-list integration.
			if item.Parent == nil && item.Deleted {
				if offset > 0 {
					item.ID.Clock += uint64(offset)
					item.Content = item.Content.Splice(offset)
				}
				txn.doc.store.Append(item)
				existingEnd = itemEnd // track progress so subsequent items in the group are not falsely gapped
				clock = itemEnd
				continue
			}

			// Items whose parent can't be resolved yet (cross-client
			// reference to a group not yet decoded) are deferred.
			if item.Parent == nil {
				pending = append(pending, item)
				clock = itemEnd
				continue
			}

			// Resolve left neighbor from the Origin ID so that integrate()
			// starts its scan from the correct position in the linked list.
			// (Local inserts set item.Left directly; remote items only have Origin.)
			if offset == 0 && item.Origin != nil {
				item.Left = txn.doc.store.getItemCleanEnd(txn, item.Origin.Client, item.Origin.Clock)
			}

			item.integrate(txn, offset)
			existingEnd = itemEnd // track progress so subsequent items in the group are not falsely gapped
			clock = itemEnd
		}
	}

	// Retry items whose parent couldn't be resolved during the first pass
	// because their origin items were in a later client group.
	for len(pending) > 0 {
		var remaining []*Item
		for _, item := range pending {
			if item.Origin != nil {
				if oi := txn.doc.store.Find(*item.Origin); oi != nil {
					item.Parent = oi.Parent
				}
			}
			if item.Parent == nil && item.OriginRight != nil {
				if ori := txn.doc.store.Find(*item.OriginRight); ori != nil {
					item.Parent = ori.Parent
				}
			}
			// If the origin is a GC placeholder (no parent), search the
			// entire store for an item with the same ParentSub that does
			// have a parent. This handles the Yjs wire-format case where
			// deleted YMap entries become GC structs and the parent type
			// name is lost.
			if item.Parent == nil && item.ParentSub != "" {
				item.Parent = findParentForMapEntry(txn.doc.store)
			}
			if item.Parent != nil {
				if item.Origin != nil {
					item.Left = txn.doc.store.getItemCleanEnd(txn, item.Origin.Client, item.Origin.Clock)
				}
				item.integrate(txn, 0)
			} else {
				remaining = append(remaining, item)
			}
		}
		if len(remaining) == len(pending) {
			// No progress made. Partition `remaining` into two buckets:
			//   - Future-clock references -> park in store.pending for retry
			//     when the missing updates arrive (fixes #11).
			//   - Truly unresolvable (e.g. GC'd parents with lost parent info
			//     from the Yjs wire format) -> store without integration so
			//     they survive re-encoding. Matches the pre-#11 fallback.
			for _, item := range remaining {
				if client, parkedAt, isFuture := itemFutureDep(item, txn.doc.store); isFuture {
					if txn.doc.store.pending == nil {
						txn.doc.store.pending = &pendingUpdate{
							missing: make(StateVector),
						}
					}
					txn.doc.store.pending.items = append(txn.doc.store.pending.items, item)
					mergePendingMissing(txn.doc.store.pending.missing, client, parkedAt)
				} else {
					txn.doc.store.Append(item)
				}
			}
			break
		}
		pending = remaining
	}

	ds, err := decodeDeleteSet(dec)
	if err != nil {
		return wrapUpdateErr(err)
	}
	unresolvableDs := ds.applyToPartial(txn)
	if len(unresolvableDs.clients) > 0 {
		txn.doc.store.pendingDs.Merge(unresolvableDs)
	}

	// pendingDs may be drainable even if pending items aren't — integrated
	// items from this update might be targets of previously-parked deletes.
	if len(txn.doc.store.pendingDs.clients) > 0 {
		pending := txn.doc.store.pendingDs
		txn.doc.store.pendingDs = newDeleteSet()
		stillUnresolvable := pending.applyToPartial(txn)
		txn.doc.store.pendingDs = stillUnresolvable
	}

	// Drain pending items whose dependencies have been satisfied by
	// this update. Inline rather than recursive (Go's sync.Mutex is not
	// reentrant; ApplyUpdateV1 is already under d.mu via doc.Transact).
	//
	// Loop until no progress to handle chained dependencies:
	//   A arriving satisfies B; B (now integrated) satisfies C; etc.
	//
	// Matches yrs' apply_update retry gate and Yjs JS's readUpdateV2
	// recursion, but executed inline so everything integrated during
	// this call surfaces in a single OnUpdate notification.
	for txn.doc.store.pending != nil && txn.doc.store.retryable(txn.doc.store.pending.missing) {
		items := txn.doc.store.pending.items
		txn.doc.store.pending = nil

		var stillPending []*Item
		stillMissing := make(StateVector)
		progressed := false

		// Process items with panic-safety: if tryIntegrate panics, re-park
		// unprocessed items and stillPending back into store.pending so a
		// subsequent ApplyUpdate can retry them. The outer applyV1Txn recover
		// (v1.1.1) then converts the panic to an error for the caller.
		idx := 0
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Restore: anything already re-parked + the unprocessed tail.
					restore := append(stillPending, items[idx:]...)
					if len(restore) > 0 {
						txn.doc.store.pending = &pendingUpdate{items: restore, missing: stillMissing}
					}
					panic(r) // re-raise for outer recover
				}
			}()
			for idx = 0; idx < len(items); idx++ {
				item := items[idx]
				if tryIntegrate(txn, item) {
					progressed = true
				} else {
					stillPending = append(stillPending, item)
					if client, parkedAt, isFuture := itemFutureDep(item, txn.doc.store); isFuture {
						mergePendingMissing(stillMissing, client, parkedAt)
					} else if item.ID.Clock > txn.doc.store.NextClock(item.ID.Client) {
						// Same-client gap still blocks it.
						mergePendingMissing(stillMissing, item.ID.Client, txn.doc.store.NextClock(item.ID.Client))
					}
				}
			}
		}()

		if len(stillPending) > 0 {
			txn.doc.store.pending = &pendingUpdate{items: stillPending, missing: stillMissing}
		}
		// Retry pendingDs — freshly-integrated items may now be targets
		// of previously-parked delete entries.
		if progressed && len(txn.doc.store.pendingDs.clients) > 0 {
			pending := txn.doc.store.pendingDs
			txn.doc.store.pendingDs = newDeleteSet()
			stillUnresolvable := pending.applyToPartial(txn)
			txn.doc.store.pendingDs = stillUnresolvable
		}
		if !progressed {
			// No progress this pass — infinite-loop guard. Items remain parked.
			break
		}
	}

	return nil
}

func decodeItem(dec *encoding.Decoder, doc *Doc, client ClientID, clock uint64) (*Item, error) {
	info, err := dec.ReadUint8()
	if err != nil {
		return nil, err
	}
	tag := info & 0x1F

	// GC struct (tag 0): placeholder for garbage-collected content.
	// Yjs encodes these as {info=0, VarUint(length)} — no origins, parent,
	// or content fields. They fill clock gaps in the store.
	if tag == 0 {
		length, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		return &Item{
			ID:      ID{Client: client, Clock: clock},
			Content: NewContentDeleted(int(length)),
			Deleted: true,
		}, nil
	}

	// Skip struct (tag 10): clock-range placeholder the sender intentionally
	// omits. Wire format: {info, VarUint(length)}. Not stored in the document.
	if tag == 10 {
		length, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		return &Item{
			ID:      ID{Client: client, Clock: clock},
			Content: &contentSkip{length: int(length)},
		}, nil
	}

	hasOrigin := info&flagHasOrigin != 0
	hasRightOrigin := info&flagHasRightOrigin != 0
	hasParentSub := info&flagHasParentSub != 0

	var origin, originRight *ID

	if hasOrigin {
		oc, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		ok, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		origin = &ID{Client: ClientID(oc), Clock: ok}
	}

	if hasRightOrigin {
		rc, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		rk, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		originRight = &ID{Client: ClientID(rc), Clock: rk}
	}

	var parent *abstractType
	var parentSub string

	if !hasOrigin && !hasRightOrigin {
		// Explicit parent info.
		parentInfo, err := dec.ReadUint8()
		if err != nil {
			return nil, err
		}
		if parentInfo == 1 {
			// Named root type.
			name, err := dec.ReadVarString()
			if err != nil {
				return nil, err
			}
			parent = doc.getOrCreateType(name)
		} else {
			// Nested type: referenced by container item's ID.
			pc, err := dec.ReadVarUint()
			if err != nil {
				return nil, err
			}
			pk, err := dec.ReadVarUint()
			if err != nil {
				return nil, err
			}
			parentItem := doc.store.Find(ID{Client: ClientID(pc), Clock: pk})
			if parentItem == nil {
				return nil, fmt.Errorf("parent item {%d,%d} not found", pc, pk)
			}
			ct, ok := parentItem.Content.(*ContentType)
			if !ok {
				return nil, fmt.Errorf("parent item {%d,%d} is not a ContentType", pc, pk)
			}
			parent = ct.Type
		}
	}

	if hasParentSub {
		parentSub, err = dec.ReadVarString()
		if err != nil {
			return nil, err
		}
	}

	content, err := decodeContent(dec, doc, tag)
	if err != nil {
		return nil, err
	}

	item := &Item{
		ID:          ID{Client: client, Clock: clock},
		Origin:      origin,
		OriginRight: originRight,
		Parent:      parent,
		ParentSub:   parentSub,
		Content:     content,
	}

	// Infer parent from origin items when not set by explicit parent info.
	if item.Parent == nil {
		if origin != nil {
			if oi := doc.store.Find(*origin); oi != nil {
				item.Parent = oi.Parent
			}
		} else if originRight != nil {
			if ori := doc.store.Find(*originRight); ori != nil {
				item.Parent = ori.Parent
			}
		}
	}

	// item.Parent may be nil when origin items belong to a client group not
	// yet decoded in this update. The caller retries these after all groups.
	return item, nil
}

func decodeContent(dec *encoding.Decoder, doc *Doc, tag byte) (Content, error) {
	switch tag {
	case wireDeleted:
		n, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		return NewContentDeleted(int(n)), nil

	case wireJSON:
		n, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		if n > uint64(dec.Remaining()) {
			return nil, ErrInvalidUpdate
		}
		vals := make([]any, n)
		for i := range vals {
			if vals[i], err = dec.ReadAny(); err != nil {
				return nil, err
			}
		}
		return NewContentJSON(vals...), nil

	case wireBinary:
		b, err := dec.ReadVarBytes()
		if err != nil {
			return nil, err
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		return NewContentBinary(cp), nil

	case wireString:
		s, err := dec.ReadVarString()
		if err != nil {
			return nil, err
		}
		return NewContentString(s), nil

	case wireEmbed:
		v, err := dec.ReadAny()
		if err != nil {
			return nil, err
		}
		return NewContentEmbed(v), nil

	case wireFormat:
		key, err := dec.ReadVarString()
		if err != nil {
			return nil, err
		}
		js, err := dec.ReadVarString()
		if err != nil {
			return nil, err
		}
		val, err := fmtValFromJSON(js)
		if err != nil {
			return nil, err
		}
		return NewContentFormat(key, val), nil

	case wireType:
		typeClass, err := dec.ReadUint8()
		if err != nil {
			return nil, err
		}
		at, err := decodeTypeContent(dec, doc, typeClass)
		if err != nil {
			return nil, err
		}
		return NewContentType(at), nil

	case wireAny:
		n, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		if n > uint64(dec.Remaining()) {
			return nil, ErrInvalidUpdate
		}
		vals := make([]any, n)
		for i := range vals {
			if vals[i], err = dec.ReadAny(); err != nil {
				return nil, err
			}
		}
		return NewContentAny(vals...), nil

	case wireDoc:
		guidBytes, err := dec.ReadVarBytes()
		if err != nil {
			return nil, err
		}
		guid := string(guidBytes)
		return NewContentDoc(New(WithGUID(guid))), nil

	case wireMove:
		clientU, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		clock, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		targetLen, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		target := &ID{Client: ClientID(clientU), Clock: clock}
		return NewContentMove(target, int(targetLen)), nil

	default:
		return nil, fmt.Errorf("unknown content tag: %d", tag)
	}
}

func decodeTypeContent(dec *encoding.Decoder, doc *Doc, typeClass byte) (*abstractType, error) {
	switch typeClass {
	case 0: // YArray
		arr := &YArray{}
		arr.doc = doc
		arr.itemMap = make(map[string]*Item)
		arr.owner = arr
		return &arr.abstractType, nil

	case 1: // YMap
		m := &YMap{}
		m.doc = doc
		m.itemMap = make(map[string]*Item)
		m.owner = m
		return &m.abstractType, nil

	case 2: // YText
		txt := &YText{}
		txt.doc = doc
		txt.itemMap = make(map[string]*Item)
		txt.owner = txt
		return &txt.abstractType, nil

	case 3: // YXmlElement
		nodeName, err := dec.ReadVarString()
		if err != nil {
			return nil, err
		}
		elem := NewYXmlElement(nodeName)
		elem.doc = doc
		return &elem.abstractType, nil

	case 4: // YXmlFragment
		frag := &YXmlFragment{}
		frag.doc = doc
		frag.itemMap = make(map[string]*Item)
		frag.owner = frag
		return &frag.abstractType, nil

	case 6: // YXmlText
		xt := NewYXmlText()
		xt.doc = doc
		return &xt.abstractType, nil

	default:
		// Unknown type class: placeholder rawType.
		r := &rawType{}
		r.doc = doc
		r.itemMap = make(map[string]*Item)
		r.owner = r
		return &r.abstractType, nil
	}
}

func decodeDeleteSet(dec *encoding.Decoder) (DeleteSet, error) {
	ds := newDeleteSet()
	n, err := dec.ReadVarUint()
	if err != nil {
		return ds, err
	}
	if n > maxV2Items {
		return ds, ErrInvalidUpdate
	}
	for i := uint64(0); i < n; i++ {
		clientU, err := dec.ReadVarUint()
		if err != nil {
			return ds, err
		}
		numRanges, err := dec.ReadVarUint()
		if err != nil {
			return ds, err
		}
		if numRanges > maxV2Items {
			return ds, ErrInvalidUpdate
		}
		client := ClientID(clientU)
		for j := uint64(0); j < numRanges; j++ {
			clock, err := dec.ReadVarUint()
			if err != nil {
				return ds, err
			}
			length, err := dec.ReadVarUint()
			if err != nil {
				return ds, err
			}
			ds.clients[client] = append(ds.clients[client], DeleteRange{Clock: clock, Len: length})
		}
	}
	for c := range ds.clients {
		ds.sortAndCompact(c)
	}
	return ds, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func clientsSorted[T any](m map[ClientID]T) []ClientID {
	out := make([]ClientID, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func wrapUpdateErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrInvalidUpdate, err)
}

// fmtValToJSON serialises a ContentFormat attribute value as a JSON string,
// matching Yjs's ContentFormat.write() which calls encoder.writeJSON(value).
func fmtValToJSON(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// fmtValFromJSON deserialises a ContentFormat attribute value from a JSON
// string, matching Yjs's ContentFormat.read() which calls decoder.readJSON().
// Numbers decode as float64, booleans as bool, null as nil.
func fmtValFromJSON(s string) (any, error) {
	if s == "undefined" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// tryIntegrate attempts to integrate item into the doc store. Returns
// true on success (item is now integrated into the linked list, or
// stored as an orphan when its parent is truly unresolvable). Returns
// false if blocked on a future-clock dependency — the caller should
// leave it in the pending queue.
//
// Used by the inline retry loop to drain pending items that may now
// be integrable. Parallels the normal decode-loop path but with items
// that have already been decoded.
func tryIntegrate(txn *Transaction, item *Item) bool {
	store := txn.doc.store

	existingEnd := store.NextClock(item.ID.Client)

	// Already fully integrated (arrived twice somehow): drop silently.
	if item.ID.Clock+uint64(item.Content.Len()) <= existingEnd {
		return true
	}

	// Same-client clock gap: still blocked.
	if item.ID.Clock > existingEnd {
		return false
	}

	// GC-orphan path (no parent, deleted): store without integration.
	if item.Parent == nil && item.Deleted {
		store.Append(item)
		return true
	}

	// Try to resolve parent via Origin / OriginRight / ParentSub fallback.
	if item.Parent == nil {
		if item.Origin != nil {
			if oi := store.Find(*item.Origin); oi != nil {
				item.Parent = oi.Parent
			} else if item.Origin.Clock >= store.NextClock(item.Origin.Client) {
				return false // future clock — still blocked
			}
		}
		if item.Parent == nil && item.OriginRight != nil {
			if ori := store.Find(*item.OriginRight); ori != nil {
				item.Parent = ori.Parent
			} else if item.OriginRight.Clock >= store.NextClock(item.OriginRight.Client) {
				return false
			}
		}
		if item.Parent == nil && item.ParentSub != "" {
			item.Parent = findParentForMapEntry(store)
		}
		if item.Parent == nil {
			// Truly unresolvable — orphan store (existing behavior).
			store.Append(item)
			return true
		}
	}

	// Origin present but referring to a future clock -> still blocked.
	if item.Origin != nil && item.Origin.Clock >= store.NextClock(item.Origin.Client) {
		return false
	}

	// Resolve left neighbor for integrate().
	if item.Origin != nil {
		item.Left = store.getItemCleanEnd(txn, item.Origin.Client, item.Origin.Clock)
	}

	item.integrate(txn, 0)
	return true
}

// itemFutureDep reports whether item is blocked on a future-clock dependency
// (one whose referenced clock has not yet been integrated into the store).
// Returns (missingClient, storeClockAtParkTime, true) if yes; otherwise
// (0, 0, false) indicating the item's parent is truly unresolvable (e.g.
// origin references a GC placeholder whose parent info was lost).
func itemFutureDep(item *Item, store *StructStore) (ClientID, uint64, bool) {
	if item.Origin != nil {
		storeClock := store.NextClock(item.Origin.Client)
		if item.Origin.Clock >= storeClock {
			return item.Origin.Client, storeClock, true
		}
	}
	if item.OriginRight != nil {
		storeClock := store.NextClock(item.OriginRight.Client)
		if item.OriginRight.Clock >= storeClock {
			return item.OriginRight.Client, storeClock, true
		}
	}
	return 0, 0, false
}
