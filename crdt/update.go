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

// MergeUpdatesV1 and DiffUpdateV1 live in merge.go — they operate at the struct
// level (preserving non-integrable structs) rather than integrate-then-reencode.

// EncodeStateVectorV1 serialises the document's state vector as a compact
// binary blob (VarUint count, then client/clock pairs).
func EncodeStateVectorV1(doc *Doc) []byte {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	sv := doc.store.StateVector()
	clients := clientsSorted(sv)
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(len(clients)))
		for _, c := range clients {
			enc.WriteVarUint(uint64(c))
			enc.WriteVarUint(sv[c])
		}
	})
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

	deleteSet := buildDeleteSet(doc.store)

	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
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
		encodeDeleteSet(enc, deleteSet)
	})
}

func encodeItem(enc *encoding.Encoder, item *Item, offset int, store *StructStore) {
	// Orphaned items (no parent) came from GC wire format where the parent
	// type name is lost. Encode them as GC structs so receivers get valid
	// clock accounting instead of corrupt data.
	//
	// Require no origins too: a struct-level-decoded item (MergeUpdatesV1 /
	// DiffUpdateV1) that infers its parent from an origin legitimately has a nil
	// Parent but a valid Origin/OriginRight — it must be encoded as a normal item
	// (origin + content, no parent info), NOT as a GC placeholder, or its content
	// would be lost. Parent info is only written in the no-origin branch below,
	// so an origin-bearing item never dereferences item.Parent. (#125)
	if item.Parent == nil && item.Origin == nil && item.OriginRight == nil {
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
	if item.ParentSub != nil {
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

	// Parent info + parentSub — only when neither origin is present. This
	// mirrors Yjs's Item.write exactly: the BIT6 "hasParentSub" flag is set
	// in the info byte whenever ParentSub is present (see above), but the
	// parentSub STRING is written only inside the no-origin block. When an
	// origin IS present the receiver inherits parentSub from the left/origin
	// item at integration time, so writing it here would put bytes on the
	// wire that a conformant (Yjs) decoder does not expect. (#YMap-wire)
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

		if item.ParentSub != nil {
			enc.WriteVarString(*item.ParentSub)
		}
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
		// Emit only the tail from `offset`. splitUTF16 emits a leading U+FFFD
		// when offset bisects a surrogate pair, matching Yjs's mid-surrogate slice.
		_, tail := splitUTF16(ct.Str, offset)
		enc.WriteVarString(tail)
	case *ContentEmbed:
		// Yjs V1 encodes the embed via writeJSON = writeVarString(JSON.stringify).
		// (V2 uses writeAny — see encodeContentV2.) Using WriteAny here put a
		// lib0-tagged value on the wire that genuine Yjs decodes as a JSON
		// string → failure. (#wire-conformance)
		enc.WriteVarString(fmtValToJSON(ct.Val))
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
		enc.WriteVarString(guid)
		// Yjs ContentDoc.write emits guid THEN writeAny(opts). opts is always
		// an object (defaults to {}); omitting it desyncs a Yjs decoder, and a
		// `null` makes Yjs crash on opts.shouldLoad. ygo doesn't track subdoc
		// opts, so emit an empty object. (#wire-conformance)
		enc.WriteAny(map[string]any{})
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

	// 1. Decode all items, parking any with future-clock deps or same-client gaps.
	//    Returns the within-update pending list (items whose parent might resolve
	//    later in this same update).
	withinUpdatePending, err := decodeAndPark(txn, dec, sv, numClients)
	if err != nil {
		return wrapUpdateErr(err)
	}

	// 2. Within-update retry pass: items whose parents arrived later in this update.
	if err := resolveWithinUpdatePending(txn, withinUpdatePending); err != nil {
		return err
	}

	ds, err := decodeDeleteSet(dec)
	if err != nil {
		return wrapUpdateErr(err)
	}
	unresolvableDs := ds.applyToPartial(txn)
	if len(unresolvableDs.clients) > 0 {
		txn.doc.store.pendingDs.Merge(unresolvableDs)
	}

	drainPending(txn)

	return nil
}

// decodeAndPark walks the V1 update's per-client struct groups, decodes each
// item, and either:
//
//	(a) skips fully-integrated items
//	(b) parks items with same-client clock gaps in store.pending
//	(c) integrates GC items directly via store.Append
//	(d) integrates items whose parent is known via item.integrate
//	(e) collects items whose parent is unresolved-but-might-be-in-this-update
//	    into the returned slice for the within-update retry pass.
//
// numClients is the count parsed from the header.
func decodeAndPark(txn *Transaction, dec *encoding.Decoder, sv StateVector, numClients uint64) ([]*Item, error) {
	var pending []*Item

	totalStructs := uint64(0)
	for i := uint64(0); i < numClients; i++ {
		numStructs, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		totalStructs += numStructs
		if totalStructs > maxV2Items {
			return nil, ErrInvalidUpdate
		}
		clientU, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		client := ClientID(clientU)
		clock, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}

		existingEnd := sv.Clock(client)

		for j := uint64(0); j < numStructs; j++ {
			item, err := decodeItem(dec, txn.doc, client, clock)
			if err != nil {
				return nil, err
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
				if txn.doc.store.pending != nil && len(txn.doc.store.pending.items) >= txn.doc.maxPendingItemsLimit() {
					return nil, wrapUpdateErr(ErrInvalidUpdate)
				}
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

			// OriginRight referencing a not-yet-integrated clock: defer (Yjs
			// getMissing parity). A root type's parent is resolved by name, so
			// such an item is NOT caught by the parent==nil defer above; without
			// this, an item whose right origin is a missing concurrent client
			// would integrate at the wrong position — permanent divergence
			// (review finding C-2). Deferral routes it through the within-update
			// retry and, failing that, store.pending via itemFutureDep. Only at
			// offset==0: a split (offset>0) overlaps an already-present item, so
			// its origins are necessarily satisfied.
			if offset == 0 && item.OriginRight != nil &&
				item.OriginRight.Clock >= txn.doc.store.NextClock(item.OriginRight.Client) {
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

	return pending, nil
}

// resolveWithinUpdatePending takes the items deferred during decoding
// (those whose parent might resolve via later items in the same update)
// and runs a fixed-point loop: try to integrate each item by resolving its
// parent from the store. If progress was made, try again with the remaining
// items. When no progress is made, partition survivors into future-clock
// (park in store.pending) vs truly-unresolvable (orphan-Append).
// Returns ErrInvalidUpdate (wrapped) if the pending queue cap is exceeded.
func resolveWithinUpdatePending(txn *Transaction, pending []*Item) error {
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
			// Parent referenced by container item-ID (review finding C-3): now
			// that earlier groups in this update have integrated, the container
			// may exist. Resolve precisely BEFORE the ParentSub fallback below,
			// which would otherwise graft this keyed item onto an arbitrary map.
			if item.Parent == nil && item.parentID != nil {
				if pi := txn.doc.store.Find(*item.parentID); pi != nil {
					if ct, ok := pi.Content.(*ContentType); ok {
						item.Parent = ct.Type
					}
				}
			}
			// If the origin is a GC placeholder (no parent), search the
			// entire store for an item with the same ParentSub that does
			// have a parent. This handles the Yjs wire-format case where
			// deleted YMap entries become GC structs and the parent type
			// name is lost.
			if item.Parent == nil && item.parentID == nil && item.ParentSub != nil {
				item.Parent = findParentForMapEntry(txn.doc.store)
			}
			if item.Parent != nil {
				// A resolved parent is not sufficient: the item may still depend
				// on a not-yet-integrated origin/rightOrigin clock (review finding
				// C-2). Integrating now would place it at the wrong position
				// (permanent divergence). Defer it to `remaining` so the
				// no-progress branch below parks it via itemFutureDep for retry
				// when the missing client arrives.
				if _, _, isFuture := itemFutureDep(item, txn.doc.store); isFuture {
					remaining = append(remaining, item)
					continue
				}
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
					if txn.doc.store.pending != nil && len(txn.doc.store.pending.items) >= txn.doc.maxPendingItemsLimit() {
						return wrapUpdateErr(ErrInvalidUpdate)
					}
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
	return nil
}

// drainPending runs the doc-level pending drain. It first retries any
// previously-parked delete-set entries (pendingDs), then loops over
// store.pending as long as progress is being made, integrating items and
// re-parking those still blocked. A panic-safety closure re-parks any
// unprocessed items back into store.pending before re-raising, so the
// outer applyV1Txn recover (v1.1.1) can convert the panic to an error.
// Also retries pendingDs after each successful integration pass.
func drainPending(txn *Transaction) {
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
	var parentSub *string
	var parentByID *ID // set when the parent is referenced by ID but not yet integrated

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
				// Container not integrated yet. EncodeStateAsUpdate sorts groups
				// ascending by client, so a lower-clientID item written into a
				// higher-clientID peer's nested type decodes before its parent.
				// Defer instead of hard-failing (Yjs pendingStructs parity,
				// review finding C-3): record the parent ID and let integration
				// resolve it once the container arrives.
				parentByID = &ID{Client: ClientID(pc), Clock: pk}
			} else if ct, ok := parentItem.Content.(*ContentType); ok {
				parent = ct.Type
			} else {
				return nil, fmt.Errorf("parent item {%d,%d} is not a ContentType", pc, pk)
			}
		}
	}

	// parentSub is on the wire ONLY when neither origin is present (Yjs writes
	// it inside the `origin==null && rightOrigin==null` block). When an origin
	// IS present the BIT6 flag may still be set, but no string follows —
	// parentSub is inherited from the left/origin item below. Reading a string
	// here unconditionally (on BIT6 alone) consumes content bytes and
	// misaligns the decoder → "unknown Any tag" / EOF on V1, silent data loss
	// on the keyed item. (#YMap-wire)
	if hasParentSub && !hasOrigin && !hasRightOrigin {
		sub, serr := dec.ReadVarString()
		if serr != nil {
			return nil, serr
		}
		parentSub = &sub
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
		parentID:    parentByID,
		Content:     content,
	}

	// Infer parent (and parentSub) from origin items when not set by explicit
	// parent info. A keyed item written after the same key (LWW) carries an
	// origin and therefore no on-wire parentSub; it inherits the key from its
	// left/origin neighbour, exactly as Yjs resolves it during integration.
	// Without inheriting ParentSub here the item would integrate with an empty
	// key and never land in the parent's itemMap → silent loss. (#YMap-wire)
	if item.Parent == nil {
		if origin != nil {
			if oi := doc.store.Find(*origin); oi != nil {
				item.Parent = oi.Parent
				if item.ParentSub == nil {
					item.ParentSub = oi.ParentSub
				}
			}
		} else if originRight != nil {
			if ori := doc.store.Find(*originRight); ori != nil {
				item.Parent = ori.Parent
				if item.ParentSub == nil {
					item.ParentSub = ori.ParentSub
				}
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
		// V1: embed is a JSON-text varstring (Yjs writeJSON), not a lib0 Any.
		js, err := dec.ReadVarString()
		if err != nil {
			return nil, err
		}
		v, err := fmtValFromJSON(js)
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
		// Consume the opts object Yjs writes after the guid (writeAny). ygo
		// doesn't use subdoc opts yet, but the bytes MUST be read or the
		// struct stream desyncs. (#wire-conformance)
		if _, err := dec.ReadAny(); err != nil {
			return nil, err
		}
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

	case 5: // YXmlHook — ygo has no hook type, but Yjs writes a hookName
		// string after the ref. We MUST consume it (mirroring decodeTypeContentV2)
		// or the rest of the update stream desyncs. Degrade to a rawType
		// placeholder. (#wire-conformance)
		if _, err := dec.ReadVarString(); err != nil {
			return nil, err
		}
		r := &rawType{}
		r.doc = doc
		r.itemMap = make(map[string]*Item)
		r.owner = r
		return &r.abstractType, nil

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
	// As in the first-pass decode, a keyed item that arrived with an origin
	// has no on-wire parentSub and must inherit the key from its origin
	// neighbour here too, or it integrates keyless and is dropped from the
	// parent's itemMap. (#YMap-wire)
	if item.Parent == nil {
		if item.Origin != nil {
			if oi := store.Find(*item.Origin); oi != nil {
				item.Parent = oi.Parent
				if item.ParentSub == nil {
					item.ParentSub = oi.ParentSub
				}
			} else if item.Origin.Clock >= store.NextClock(item.Origin.Client) {
				return false // future clock — still blocked
			}
		}
		if item.Parent == nil && item.OriginRight != nil {
			if ori := store.Find(*item.OriginRight); ori != nil {
				item.Parent = ori.Parent
				if item.ParentSub == nil {
					item.ParentSub = ori.ParentSub
				}
			} else if item.OriginRight.Clock >= store.NextClock(item.OriginRight.Client) {
				return false
			}
		}
		// Parent referenced by container item-ID (review finding C-3): resolve
		// precisely if the container has arrived; if it references a future
		// clock, park (return false) rather than grafting onto an arbitrary map
		// via the ParentSub fallback below.
		if item.Parent == nil && item.parentID != nil {
			if pi := store.Find(*item.parentID); pi != nil {
				if ct, ok := pi.Content.(*ContentType); ok {
					item.Parent = ct.Type
				}
			} else if item.parentID.Clock >= store.NextClock(item.parentID.Client) {
				return false
			}
		}
		if item.Parent == nil && item.parentID == nil && item.ParentSub != nil {
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

	// OriginRight referring to a future clock -> still blocked. Yjs's getMissing
	// blocks integration on origin, rightOrigin, OR parent; ygo only checked
	// OriginRight inside the Parent==nil branch above. A root type's parent is
	// resolved by name, so that branch is skipped and an item whose right origin
	// is a not-yet-integrated client would otherwise integrate at the wrong
	// position (permanent divergence, review finding C-2). itemFutureDep already
	// reports OriginRight as the missing dependency, so the item parks here and
	// retries once that client's clock advances.
	if item.OriginRight != nil && item.OriginRight.Clock >= store.NextClock(item.OriginRight.Client) {
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
	// Parent referenced by an as-yet-unintegrated container item (review
	// finding C-3): park on the container's client until it arrives.
	if item.Parent == nil && item.parentID != nil {
		storeClock := store.NextClock(item.parentID.Client)
		if item.parentID.Clock >= storeClock {
			return item.parentID.Client, storeClock, true
		}
	}
	return 0, 0, false
}
