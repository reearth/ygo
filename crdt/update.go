package crdt

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
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

// ErrIntegrateScanBudget, which wraps ErrInvalidUpdate as the pending cap's
// error does, is returned by a remote apply whose conflict scans would pass the
// bound WithIntegrateScanBudget set for its origin. The apply aborted partway
// and the document keeps what it integrated, which ApplyUpdateV1Landed reports,
// and the same update sent again carries on from there until an item whose
// scan alone passes the bound, where each send from then on stops with nothing
// landed; a document with WithWholeOrNothingApplies keeps nothing of the apply.
var ErrIntegrateScanBudget = fmt.Errorf("%w: its conflict scans pass the document's bound", ErrInvalidUpdate)

// integrateScanBudgetExceeded is the panic Item.integrate raises when a
// bounded apply's scans reach the bound. left is what the bound left the scan
// that tripped when it began: the item needs more than that.
type integrateScanBudgetExceeded struct{ left uint32 }

// drainStopped is the panic drainPending raises again, for drainParked, when
// the scan bound, the apply budget or the allowed roots stop the drain rather
// than fail it. scan is whether the scan bound stopped it, and left is then
// what the bound left the scan that tripped; dropped is the error of a parked
// struct the drain dropped before it stopped.
type drainStopped struct {
	scan    bool
	left    uint32
	dropped error
}

// drainStop is the drainStopped r stands for, when r stops a drain.
func drainStop(r any) (drainStopped, bool) {
	switch r := r.(type) {
	case integrateScanBudgetExceeded:
		return drainStopped{scan: true, left: r.left}, true
	case applyBudgetExceeded, rootNotAllowed:
		return drainStopped{}, true
	}
	return drainStopped{}, false
}

// ── Public API ────────────────────────────────────────────────────────────────

// EncodeStateAsUpdateV1 encodes the part of doc newer than sv as a V1 binary
// update. Pass nil to encode the entire document state.
func EncodeStateAsUpdateV1(doc *Doc, sv StateVector) []byte {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	return withParked(doc, sv, encodeV1Locked(doc, sv), encodeStructStoreV1, MergeUpdatesV1)
}

// ApplyUpdateV1 decodes and integrates a V1 binary update into doc.
// When it changes formatted text, a local transaction with a nil origin may
// follow it, once its observers have fired, to clean up the format markers it
// left redundant, as in Yjs, with its own update and observer callbacks.
func ApplyUpdateV1(doc *Doc, update []byte, origin any) error {
	bound := doc.scanBudget(origin)
	var applyErr error
	doc.Transact(func(txn *Transaction) {
		txn.Local = false // Yjs readUpdate: transact(..., local=false)
		txn.boundScans(bound)
		applyErr = applyV1Txn(txn, update)
	}, origin)
	return applyErr
}

// ApplyUpdateV1Landed is ApplyUpdateV1 that, when the apply fails partway, also
// returns landed, a V1 update of exactly what this call left in doc (nil if
// nothing, as on a document with WithWholeOrNothingApplies): what it integrated
// and the structs and deletions it parked, for a caller that must forward it,
// so a peer parks what doc parks. An update that overflows the pending cap or
// passes the scan bound, say, can fail partway.
func ApplyUpdateV1Landed(doc *Doc, update []byte, origin any) (landed []byte, err error) {
	bound := doc.scanBudget(origin)
	doc.Transact(func(txn *Transaction) {
		txn.Local = false // Yjs readUpdate: transact(..., local=false)
		txn.boundScans(bound)
		if err = applyV1Txn(txn, update); err != nil {
			landed = encodeTxnV1Locked(txn)
		}
	}, origin)
	return landed, err
}

// encodeTxnV1Locked encodes the structs txn has added to the store so far, the
// deletions it has made and the structs and deletions it parked, or returns nil
// when there are none. Each client's new structs are found by binary search
// from txn.beforeState, so the cost is proportional to the clients in the
// store plus what txn added, parked and took out of the queue. A client's
// parked structs follow its added ones, whole, past a skip where the store has
// a gap, so a reader parks each as the store did. Called with the document
// lock held.
func encodeTxnV1Locked(txn *Transaction) []byte {
	store := txn.doc.store
	perClient := make(map[ClientID][]*Item)
	for client, items := range store.clients {
		from := txn.beforeState.Clock(client)
		i := sort.Search(len(items), func(i int) bool {
			return items[i].ID.Clock+items[i].Content.Len() > from
		})
		if i < len(items) {
			perClient[client] = items[i:]
		}
	}
	var record transactionParked
	if store.parked != nil {
		record = *store.parked
	}
	parked := make(map[ClientID][]*Item)
	if len(record.items) > 0 && store.pending != nil {
		// Only the structs still parked: a drain took the others out.
		var left map[*Item]struct{}
		if len(record.left) > 0 {
			left = make(map[*Item]struct{}, len(record.left))
			for _, it := range record.left {
				left[it] = struct{}{}
			}
		}
		for _, it := range record.items {
			if _, gone := left[it]; it != nil && !gone {
				parked[it.ID.Client] = append(parked[it.ID.Client], it)
			}
		}
	}
	sv, svCopied := txn.beforeState, false
	ds := cloneDeleteSet(txn.deleteSet)
	for client, items := range parked {
		slices.SortFunc(items, func(a, b *Item) int { return cmp.Compare(a.ID.Clock, b.ID.Clock) })
		// Clip so the append copies rather than writing past the store's slice.
		perClient[client] = append(slices.Clip(perClient[client]), items...)
		if first := items[0].ID.Clock; first < sv.Clock(client) {
			// A parked struct that starts below what the reader holds is sent
			// whole, as the store parked it, not cut at the reader's clock.
			if !svCopied {
				sv, svCopied = maps.Clone(txn.beforeState), true
			}
			sv[client] = first
		}
	}
	// The delete ranges this apply parked travel too: a drain that fails
	// after the delete set parked them leaves them parked in doc.
	for _, parked := range record.deletes {
		ds.Merge(parked)
	}
	if len(perClient) == 0 && len(ds.clients) == 0 {
		return nil
	}
	return encodeStructStoreV1(perClient, ds, sv)
}

// EncodeStateAsUpdateV2 encodes the document state using the Yjs V2
// column-oriented binary format.  The output is interoperable with
// Y.applyUpdateV2 / Y.encodeStateAsUpdateV2 from the yjs npm package.
func EncodeStateAsUpdateV2(doc *Doc, sv StateVector) []byte {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	return withParked(doc, sv, encodeV2Locked(doc, sv), encodeStructStoreV2, MergeUpdatesV2)
}

// withParked merges structs and deletions still parked on a missing
// dependency into an encoded state, as Yjs encodeStateAsUpdate does with
// pendingStructs and pendingDs; a snapshot that dropped them would lose them
// once the filler arrives. Kept out of encodeV1Locked so OnUpdate payloads
// stay transaction-only, as in Yjs. A part that does not merge is left out
// alone, never the rest of the parked state with it.
func withParked(doc *Doc, sv StateVector, state []byte,
	encode func(map[ClientID][]*Item, DeleteSet, StateVector) []byte,
	merge func(...[]byte) ([]byte, error)) []byte {
	s := doc.store
	if s.pending == nil && len(s.pendingDs.clients) == 0 {
		return state
	}
	var parts [][]byte
	if s.pending != nil {
		// One update per item, so one that does not merge is left out alone.
		for _, it := range s.pending.heldItems() {
			parts = append(parts, encode(map[ClientID][]*Item{it.ID.Client: {it}}, newDeleteSet(), sv))
		}
	}
	// One update per client's parked ranges, so a bad range costs only its
	// client's.
	for client, ranges := range s.pendingDs.clients {
		ds := newDeleteSet()
		ds.clients[client] = ranges
		parts = append(parts, encode(nil, ds, sv))
	}
	return mergeParts(state, parts, merge)
}

// mergeParts merges parts into state, leaving out each part that does not
// merge. A merge fails only when one of its updates does not decode, so when
// the whole merge fails each update is merged alone once: a state that does
// not merge keeps no part, and a part that does not merge is left out.
func mergeParts(state []byte, parts [][]byte, merge func(...[]byte) ([]byte, error)) []byte {
	if merged, err := merge(append([][]byte{state}, parts...)...); err == nil {
		return merged
	}
	if _, err := merge(state); err != nil {
		return state
	}
	good := [][]byte{state}
	for _, p := range parts {
		if _, err := merge(p); err == nil {
			good = append(good, p)
		}
	}
	if merged, err := merge(good...); err == nil {
		return merged
	}
	return state
}

// ApplyUpdateV2 decodes and integrates a Yjs V2 binary update into doc.
// When it changes formatted text, a local transaction with a nil origin may
// follow it, once its observers have fired, to clean up the format markers it
// left redundant, as in Yjs, with its own update and observer callbacks.
func ApplyUpdateV2(doc *Doc, update []byte, origin any) error {
	bound := doc.scanBudget(origin)
	var applyErr error
	doc.Transact(func(txn *Transaction) {
		txn.Local = false // Yjs readUpdate: transact(..., local=false)
		txn.boundScans(bound)
		applyErr = applyV2Txn(txn, update)
	}, origin)
	return applyErr
}

// UpdateV1ToV2 converts a V1 update payload to real Yjs V2 format. It works at
// the struct level, like MergeUpdatesV1, so an incremental or delete-only
// update converts intact (integrating into a scratch doc parked such updates
// and emitted an empty one; yjs convertUpdateFormatV1ToV2 parity).
func UpdateV1ToV2(v1 []byte) ([]byte, error) {
	perClient, ds, err := buildMergeStore([][]byte{v1}, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV2(perClient, ds, StateVector{}), nil
}

// UpdateV2ToV1 converts a real Yjs V2 update to V1 format, at the struct level
// (see UpdateV1ToV2).
func UpdateV2ToV1(v2 []byte) ([]byte, error) {
	perClient, ds, err := buildMergeStore([][]byte{v2}, decodeStructsV2)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV1(perClient, ds, StateVector{}), nil
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
			if item.ID.Clock+item.Content.Len() > svClock {
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
				offset := uint64(0)
				if i == 0 && g.startClock > item.ID.Clock {
					offset = g.startClock - item.ID.Clock
				}
				encodeItem(enc, item, offset)
			}
		}
		encodeDeleteSet(enc, deleteSet)
	})
}

// isGCOrphan reports whether item is a GC struct: no parent, origins or parent
// ID. An item whose parent is only unresolved keeps an origin or parent ID.
func (item *Item) isGCOrphan() bool {
	return item.Parent == nil && item.Origin == nil && item.OriginRight == nil && item.parentID == nil
}

// markGCOrphan turns item into the GC struct Yjs makes when its parent cannot
// be resolved, a neighbour is a GC struct, or its nested type is collected:
// deleted, length-only content, and no parent, origins, neighbours or redo
// link. It then encodes as a GC struct, and nothing can resolve a parent
// through it, so children that arrive later are orphaned too instead of
// integrating into a detached type. Without neighbour links, a GC struct that
// RunGC merges away is not kept in memory by the struct that absorbs it.
func (item *Item) markGCOrphan() {
	if _, ok := item.Content.(*ContentDeleted); !ok {
		item.Content = NewContentDeleted(item.Content.Len())
	}
	item.dropGCLinks()
}

// dropGCLinks makes item deleted and clears what a GC struct does not have:
// its parent, parent ID, origins, neighbours and redo link.
func (item *Item) dropGCLinks() {
	item.Deleted = true
	item.Parent = nil
	item.ParentSub = nil
	item.parentID = nil
	item.Origin = nil
	item.OriginRight = nil
	item.Left = nil
	item.Right = nil
	item.redone = nil
}

func encodeItem(enc *encoding.Encoder, item *Item, offset uint64) {
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
	//
	// Also exclude items carrying a deferred parent-by-ID: they have a nil Parent
	// (not yet integrated) but a known container ID in parentID, and must be
	// encoded as a normal item so the no-origin branch below can re-emit the
	// explicit parent-by-ID. Treating them as GC orphans here dropped their
	// content and parent link (silent data loss on merge). (#140)
	if item.Parent == nil && item.Origin == nil && item.OriginRight == nil && item.parentID == nil {
		length := item.Content.Len()
		if offset > 0 {
			length -= offset
		}
		enc.WriteUint8(0) // GC struct info byte
		enc.WriteVarUint(length)
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
		oc := item.ID.Clock + offset - 1
		origin = &ID{Client: item.ID.Client, Clock: oc}
		originRight = item.OriginRight
	} else {
		origin = item.Origin
		originRight = item.OriginRight
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
		} else if item.parentID != nil {
			// Deferred parent-by-ID: this item was decoded but not yet
			// integrated (its container is in a later client group), so Parent
			// is still nil while parentID holds the container's ID. Re-emit the
			// explicit parent-by-ID so the receiver can defer and resolve it too.
			enc.WriteUint8(0)
			enc.WriteVarUint(uint64(item.parentID.Client))
			enc.WriteVarUint(item.parentID.Clock)
		} else {
			// Root named type; an item with no parent is a GC struct, written above.
			enc.WriteUint8(1)
			enc.WriteVarString(item.Parent.name)
		}

		if item.ParentSub != nil {
			enc.WriteVarString(*item.ParentSub)
		}
	}

	encodeContent(enc, item.Content, offset)
}

func encodeContent(enc *encoding.Encoder, c Content, offset uint64) {
	switch ct := c.(type) {
	case *ContentDeleted:
		enc.WriteVarUint(ct.length - offset)
	case *ContentJSON:
		vals := ct.Vals[offset:]
		enc.WriteVarUint(uint64(len(vals)))
		for _, v := range vals {
			// Yjs writes JSON text per value in V1 too, not lib0 Any.
			enc.WriteVarString(fmtValToJSON(v))
		}
	case *ContentBinary:
		enc.WriteVarBytes(ct.Data)
	case *ContentString:
		// Emit only the tail from `offset`. splitUTF16 emits a leading U+FFFD
		// when offset bisects a surrogate pair, matching Yjs's mid-surrogate slice.
		_, tail := splitUTF16(ct.Str, int(min(offset, uint64(ct.utf16Len))))
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
		tc, name := typeClassOf(ct)
		enc.WriteUint8(tc)
		if typeRefHasName(tc) {
			enc.WriteVarString(name)
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
		writeSubdocOpts(enc, ct.Doc)
	case *ContentMove:
		enc.WriteVarUint(uint64(ct.Target.Client))
		enc.WriteVarUint(ct.Target.Clock)
		enc.WriteVarUint(ct.TargetLen)
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
	case *yxmlHook:
		return 5, v.hookName
	case *YXmlText:
		return 6, ""
	default:
		return 0, ""
	}
}

// typeRefHasName reports whether a type ref is followed by a name: the node
// name of a YXmlElement or the hook name of a YXmlHook.
func typeRefHasName(ref byte) bool { return ref == 3 || ref == 5 }

// maxTypeRef is YXmlText, the highest type ref Yjs defines.
const maxTypeRef = 6

// typeRefError refuses a type ref Yjs has no type for, as Yjs readContentType
// does; it holds no reference into the decoder that read it.
type typeRefError uint64

func (e typeRefError) Error() string { return fmt.Sprintf("unknown type ref: %d", uint64(e)) }

// errZeroLengthStruct refuses an item or GC struct that spans no clock: Yjs
// never writes one, and Yjs integrates a keyed one as the key's value with no
// content, which loses the key and breaks its later encoding.
var errZeroLengthStruct = errors.New("zero-length struct")

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
	// Yjs writes delete set clients in descending order, and applies a delete
	// set in its encoded order.
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
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
	applying := false
	defer func() {
		if r := recover(); r != nil {
			retErr = recoveredApplyErr(r)
		}
		// An apply that fails before it began to integrate must tell no observer.
		txn.refused = retErr != nil && !applying
		if applying {
			retErr = drainAfterApply(txn, retErr)
		}
		endRemoteApply(txn, retErr != nil)
		retErr = endApplyTally(txn, retErr)
	}()

	dec := encoding.NewDecoder(update)

	// Snapshot state vector before applying anything (used for skip/offset logic).
	sv := txn.doc.store.StateVector()
	beginRemoteApply(txn, sv)
	beginApplyTally(txn)

	numClients, err := dec.ReadVarUint()
	if err != nil {
		return wrapUpdateErr(err)
	}
	if numClients > uint64(len(update)/2) || numClients > maxV2Items {
		return ErrInvalidUpdate
	}
	// The pending cap refuses an update before the apply changes anything, for
	// its items (checkBeforeApply) or its delete ranges (checkParkedDeletes),
	// both reading the update through the apply's one budget.
	budget := newPendingBudget(txn.doc, sv, update, false)
	if numClients > 1 {
		// A later malformed group must not leave earlier, superseded groups
		// applied; the walk also counts the structs, bounding the cap check.
		if err := budget.knowSuperseded(); err != nil {
			return wrapUpdateErr(err)
		}
	} else {
		budget.setSuperseded(nil)
	}
	if err := budget.checkBeforeApply(len(update)); err != nil {
		return err
	}
	if err := checkParkedDeletes(txn.doc, &budget); err != nil {
		return err
	}

	// 1. Decode every struct; the structs of several clients all wait in
	//    pending, and a one-client update's stay in group.
	var buf [8]*Item // a keystroke's few structs stay on the stack
	withinUpdatePending, group, groupClient, err := decodeStructs(txn, dec, sv, numClients, budget.superseded, buf[:0])
	if err != nil {
		return wrapUpdateErr(err)
	}
	applying = true

	// 2. Integrate them, a one-client update in clock order and the rest in
	//    Yjs's order, and park what stays blocked.
	if numClients == 1 {
		if withinUpdatePending, err = integrateGroup(txn, group, sv.Clock(groupClient), nil); err != nil {
			return wrapUpdateErr(err)
		}
	}
	if err := resolveWithinUpdatePending(txn, withinUpdatePending); err != nil {
		return err
	}

	ds, dsRanges, dsErr := budget.deleteSet, budget.deleteSetRanges, error(nil)
	if !budget.deleteSetRead {
		ds, dsRanges, dsErr = decodeDeleteSetRanges(dec)
	}
	countDecodedRanges(txn, dsRanges)
	if err := applyDecodedDeleteSet(txn, ds, dsErr); err != nil {
		return err
	}

	return nil
}

// drainAfterApply drains the pending queue last in every apply that began to
// integrate, so one that fails partway drains on what it kept, as a peer
// applying its update does; a drain error is its error only when err is nil.
// A failed apply on a document with whole-or-nothing applies keeps nothing, so
// it drains nothing.
func drainAfterApply(txn *Transaction, err error) (retErr error) {
	retErr = err
	if err != nil && txn.doc.applying != nil {
		return err
	}
	if t := txn.doc.tally(); t != nil {
		t.draining = true
	}
	defer func() {
		if r := recover(); r != nil && retErr == nil {
			retErr = recoveredApplyErr(r)
		}
	}()
	if drainErr := drainParked(txn, drainPending); retErr == nil {
		retErr = drainErr
	}
	if txn.doc.store.pending == nil {
		// No parked struct is left to carry a list.
		txn.doc.parkedBy = nil
	}
	return retErr
}

// decodeStructs decodes every struct of the V1 update before the apply
// integrates one, as Yjs reads them all first, so a struct that does not
// decode leaves the document unchanged. The structs of an update with several
// client groups return in pending, to integrate in Yjs's order; a one-client
// update's return in group, appended to into, with its client.
// Fully integrated structs are skipped.
//
// Groups that a later group for the same client supersedes are decoded and
// dropped (yjs parity). numClients is the count parsed from the header.
func decodeStructs(txn *Transaction, dec *encoding.Decoder, sv StateVector, numClients uint64, superseded groupSet, into []*Item) (pending, group []*Item, groupClient ClientID, err error) {
	group = into
	totalStructs := uint64(0)
	for i := uint64(0); i < numClients; i++ {
		numStructs, err := dec.ReadVarUint()
		if err != nil {
			return nil, nil, 0, err
		}
		totalStructs += numStructs
		if totalStructs > maxV2Items {
			return nil, nil, 0, ErrInvalidUpdate
		}
		start, err := readID(dec)
		if err != nil {
			return nil, nil, 0, err
		}
		client, clock := start.Client, start.Clock
		groupClient = client

		existingEnd := sv.Clock(client)
		drop := superseded.has(i)
		if numClients == 1 {
			// Reserve the list up front instead of regrowing it each time it fills,
			// bounded by the bytes left since a V1 struct takes at least two.
			group = slices.Grow(group, int(min(numStructs, uint64(dec.Remaining()/2))))
		}

		kept := false // Yjs's rest takes a client's structs from one that waits
		for j := uint64(0); j < numStructs; j++ {
			// Each struct is a value the apply reads, counted before it is read.
			txn.doc.tally().decode(1)
			item, err := decodeItem(dec, txn.doc, client, clock)
			if err != nil {
				return nil, nil, 0, err
			}
			// Every struct is bounded, a replaced group's too, so apply refuses
			// what the merge decoders refuse.
			itemEnd, err := structEnd(clock, item.Content.Len())
			if err != nil {
				return nil, nil, 0, err
			}
			// Skip structs (tag 10) mark clocks the sender withheld, so the
			// receiver does NOT have them: advance the read cursor only. Items
			// after the skip then fall into the clock-gap branch and park until
			// the range arrives (yjs parity, #251).
			_, isSkip := item.Content.(*contentSkip)
			if itemEnd == clock && !isSkip {
				return nil, nil, 0, errZeroLengthStruct
			}
			switch {
			case drop || isSkip:
			case numClients > 1:
				// Yjs integrates several clients' structs highest client
				// first, whatever the wire order.
				if kept || itemEnd > existingEnd || hasMissingDep(txn.doc.store, item) || namesFutureOwn(item, existingEnd) {
					pending, kept = append(pending, item), true
				}
			default:
				group = append(group, item)
			}
			clock = itemEnd
		}
	}
	return pending, group, groupClient, nil
}

// integrateGroup integrates one client's decoded structs in clock order from
// existingEnd, the client's clock before the update, and appends to pending
// those that must wait: past a gap, or for a parent or right origin the
// document lacks.
func integrateGroup(txn *Transaction, group []*Item, existingEnd uint64, pending []*Item) ([]*Item, error) {
	store := txn.doc.store
	// Once a covered struct waits, Yjs's rest takes the client's structs from
	// there, covered or not.
	blocked := false
	for _, item := range group {
		if item.Parent == nil {
			resolveDecodedParent(store, item)
		}
		if blocked {
			pending = append(pending, item)
			continue
		}
		clock := item.ID.Clock
		itemEnd := clock + item.Content.Len()
		if itemEnd <= existingEnd {
			// Already integrated, unless it names a struct the store lacks,
			// which Yjs looks for first.
			if hasMissingDep(store, item) {
				pending, blocked = append(pending, item), true
			} else if namesFutureOwn(item, existingEnd) {
				integrateBeforeRefusal(txn, pending)
				return nil, errFutureOwnRef
			}
			continue
		}

		offset := uint64(0)
		if clock < existingEnd {
			// Partially integrated — integrate only the new suffix.
			offset = existingEnd - clock
		}

		// A predecessor this update withheld waits in the pending queue.
		if clock > existingEnd {
			pending = append(pending, item)
			continue
		}

		// GC items (tag 0) have no parent — add directly to the store
		// without linked-list integration.
		if item.Parent == nil && item.Deleted {
			if offset > 0 {
				item.ID.Clock += offset
				item.Content = item.Content.Splice(offset)
			}
			noteIntegrated(txn, item)
			txn.doc.store.Append(item)
			// A deferred move whose target is in this GC struct stops waiting.
			txn.doc.store.resolvePendingMoves(txn, item.ID.Client)
			existingEnd = itemEnd // track progress so subsequent items in the group are not falsely gapped
			continue
		}

		// Items whose parent can't be resolved yet are deferred.
		if item.Parent == nil {
			pending = append(pending, item)
			continue
		}

		// OriginRight referencing a not-yet-integrated clock: defer (Yjs
		// getMissing parity). A root type's parent is resolved by name, so
		// such an item is NOT caught by the parent==nil defer above; without
		// this, an item whose right origin is a missing concurrent client
		// would integrate at the wrong position — permanent divergence
		// (review finding C-2). Deferral routes it through the within-update
		// retry and, failing that, store.pending via itemFutureDep.
		if item.OriginRight != nil &&
			item.OriginRight.Clock >= txn.doc.store.NextClock(item.OriginRight.Client) {
			pending = append(pending, item)
			continue
		}

		if continuesGC(txn.doc.store, item, offset) {
			integrateBeforeRefusal(txn, pending)
			return nil, errGCContinuation
		}

		// Resolve left neighbor from the Origin ID so that integrate()
		// starts its scan from the correct position in the linked list.
		// (Local inserts set item.Left directly; remote items only have Origin.)
		if offset == 0 && item.Origin != nil {
			item.Left = txn.doc.store.getItemCleanEnd(txn, item.Origin.Client, item.Origin.Clock)
		}

		if rightIsGC(txn, item) {
			item.Left = nil
			item.markGCOrphan()
			appendTrimmed(txn, item, existingEnd)
			// A deferred move whose target is in this GC struct stops waiting.
			store.resolvePendingMoves(txn, item.ID.Client)
			existingEnd = itemEnd
			continue
		}
		item.integrate(txn, offset)
		existingEnd = itemEnd // track progress so subsequent items in the group are not falsely gapped
	}
	return pending, nil
}

// resolveDecodedParent sets the parent the decoder could not find because the
// struct that names it is earlier in the same group, which has integrated since.
func resolveDecodedParent(store *StructStore, item *Item) {
	if item.parentID == nil {
		if ref := parentRef(store, item); ref != nil {
			inheritParent(item, ref)
		}
		return
	}
	if container := store.Find(*item.parentID); container != nil {
		if ct, ok := container.Content.(*ContentType); ok {
			item.Parent, item.parentID = ct.Type, nil
		}
	}
}

// parentRef returns the struct whose parent it would inherit.
func parentRef(store *StructStore, it *Item) *Item {
	switch {
	case it.Origin != nil:
		return store.Find(*it.Origin)
	case it.OriginRight != nil:
		return store.Find(*it.OriginRight)
	}
	return nil
}

func inheritParent(it, ref *Item) {
	it.Parent = ref.Parent
	if it.ParentSub == nil {
		it.ParentSub = ref.ParentSub
	}
}

// resolveWithinUpdatePending integrates the items decodeStructs returned in
// Yjs's integrateStructs order (integratePending). tryIntegrate also stores
// genuine GC orphans without attaching them to a root, and respects
// same-client clock gaps. Survivors are parked in store.pending within the
// cross-update limit. Returns ErrInvalidUpdate (wrapped) if the pending queue
// cap is exceeded or a deferred item is refused.
func resolveWithinUpdatePending(txn *Transaction, pending []*Item) error {
	remaining, err := resolveDeferred(txn, pending)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return parkWithinUpdatePending(txn, remaining)
	}
	return nil
}

// resolveDeferred integrates pending in Yjs's order, dropping a refused item,
// and returns what stays blocked with the first refusal.
func resolveDeferred(txn *Transaction, pending []*Item) ([]*Item, error) {
	if len(pending) == 0 {
		return nil, nil
	}
	var buf [8]bool // a keystroke's few structs need no allocation
	done := buf[:]
	if len(pending) > len(buf) {
		done = make([]bool, len(pending))
	}
	done = done[:len(pending)]
	refused := integratePending(txn, pending, done, nil)
	remaining := pending[:0]
	for i, item := range pending {
		if !done[i] {
			remaining = append(remaining, item)
		}
	}
	return remaining, refused
}

// drainPending runs the doc-level pending drain. It first retries any
// previously-parked delete-set entries (pendingDs), then loops over
// store.pending as long as progress is being made, integrating items and
// re-parking those still blocked. A parked item whose integration panics is
// dropped, since retrying it would panic on every later drain, and the drain
// goes on; the panic is returned as an error. The scan bound, the apply budget
// and the allowed roots stop the drain instead: what the pass did not
// integrate parks again, and the stop goes on to drainParked with what the
// drain dropped (drainStopped).
// Also retries pendingDs after each successful integration pass.
func drainPending(txn *Transaction) (dropped error) {
	var items []*Item // the pass's items, until they park again
	var done []bool
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if items != nil {
			parkUndrained(txn, items, done)
		}
		if stop, ok := drainStop(r); ok {
			stop.dropped = dropped
			r = stop
		}
		panic(r)
	}()

	// pendingDs may be drainable even if pending items aren't — integrated
	// items from this update might be targets of previously-parked deletes.
	if len(txn.doc.store.pendingDs.clients) > 0 {
		retryPendingDs(txn)
	}

	// Drain pending items whose dependencies have been satisfied by
	// this update. Inline rather than recursive (Go's sync.Mutex is not
	// reentrant; the apply is already under d.mu via doc.Transact).
	//
	// Loop until no progress to handle chained dependencies:
	//   A arriving satisfies B; B (now integrated) satisfies C; etc.
	//
	// Matches yrs' apply_update retry gate and Yjs JS's readUpdateV2
	// recursion, which re-runs integrateStructs over the merged pending
	// update (integratePending keeps its order), but executed inline so
	// everything integrated during this call surfaces in a single OnUpdate
	// notification.
	for txn.drainable() {
		items = txn.doc.store.pending.heldItems()
		txn.doc.store.pending = nil

		done = make([]bool, len(items))
		// A refused parked item is dropped without failing the update, since the
		// drain cannot tell which update carried it.
		_ = integratePending(txn, items, done, &dropped)
		progressed := parkUndrained(txn, items, done)
		items = nil
		// Retry pendingDs — freshly-integrated items may now be targets
		// of previously-parked delete entries.
		if progressed && len(txn.doc.store.pendingDs.clients) > 0 {
			retryPendingDs(txn)
		}
		if !progressed {
			// No progress this pass — infinite-loop guard. Items remain parked.
			break
		}
	}
	return dropped
}

// parkUndrained parks again the items of a drain pass not marked done, and
// reports whether the pass took any out of the queue; the park record lists
// those it took.
func parkUndrained(txn *Transaction, items []*Item, done []bool) bool {
	if rec := txn.doc.store.parked; rec != nil && len(rec.items) > 0 {
		for i, it := range items {
			if done[i] {
				rec.left = append(rec.left, it)
			}
		}
	}
	stillPending, stillMissing := unresolvedPending(txn.doc.store, items, done)
	if len(stillPending) > 0 {
		txn.doc.store.pending = &pendingUpdate{items: stillPending, missing: stillMissing}
	}
	return len(stillPending) < len(items)
}

// integrationPanicErr is the error an apply returns for a panic it recovered.
func integrationPanicErr(r any) error {
	return fmt.Errorf("%w: panic during item integration: %v", ErrInvalidUpdate, r)
}

// unresolvedPending returns the items not marked done, in queue order, with
// the clocks that must advance before retrying them.
func unresolvedPending(store *StructStore, items []*Item, done []bool) ([]*Item, StateVector) {
	var remaining []*Item
	missing := make(StateVector)
	for i, item := range items {
		if done[i] {
			continue
		}
		remaining = append(remaining, item)
		if client, parkedAt, isFuture := itemFutureDep(item, store); isFuture {
			mergePendingMissing(missing, client, parkedAt)
		}
	}
	return remaining, missing
}

// drainable reports whether txn's drain should try the parked queue: a clock
// the queue waits on has advanced, or the bound tripped in the drain that last
// tried it and txn's drain has more of the bound left than the scan that
// tripped had, or no bound. A drain with no more left would trip at the same
// item again, so an item whose scan alone passes the bound costs the applies
// after it nothing until one with a larger bound, or none, tries it.
func (txn *Transaction) drainable() bool {
	p := txn.doc.store.pending
	if p == nil {
		return false
	}
	if p.tripped && (!txn.scanBounded || txn.scanLeft > p.scanLeft) {
		return true
	}
	return txn.doc.store.retryable()
}

// drainParked runs drain, the drain of the parked queue that ends an apply,
// returns its error, and raises again every panic but a stop (drainStopped);
// on a document with whole-or-nothing applies, a drain that fails or panics
// is rolled back first. When the scan bound, the apply budget or the allowed
// roots stop the drain, it keeps what it integrated, or on such a document is
// rolled back to where it began, and parks the rest again without failing the
// apply, whose sender did not send the parked items; a later apply, from any
// sender, tries them again (drainable).
func drainParked(txn *Transaction, drain func(*Transaction) error) (err error) {
	s := txn.doc.store
	j := txn.doc.applying
	// Only a drain that tries the parked items can change them or trip the bound.
	journaled := j != nil && txn.drainable()
	from := txn.scanLeft
	if journaled {
		j.beginDrain(txn)
	}
	// The parked structs this drain drops are counted only if it stands.
	var drops int
	if t := txn.doc.tally(); t != nil {
		drops = len(t.rootDrops)
	}
	defer func() {
		r := recover()
		if journaled {
			if r != nil || err != nil {
				j.takeBackDrain(txn)
				if t := txn.doc.tally(); t != nil {
					t.rootDrops = t.rootDrops[:drops]
				}
			} else {
				j.endDrain()
			}
		}
		if r == nil {
			return
		}
		stop, ok := r.(drainStopped)
		if !ok {
			panic(r)
		}
		err = stop.dropped
		if journaled {
			// Rolled back whole, the drain dropped nothing, and a drain the scan
			// bound stopped needs more than it began with.
			err = nil
			if stop.scan {
				stop.left = from
			}
		}
		if s.pending == nil {
			// A retry of the parked deletes stopped the drain, before any pass.
			return
		}
		// The drain stopped, so an item may wait on nothing but a later drain:
		// nothing tells the parked items' sender to send them again. missing
		// keeps what they do wait on.
		missing := make(StateVector)
		for _, item := range s.pending.heldItems() {
			if client, parkedAt, future := itemFutureDep(item, s); future {
				mergePendingMissing(missing, client, parkedAt)
			} else if item.ID.Clock > s.NextClock(item.ID.Client) {
				mergePendingMissing(missing, item.ID.Client, s.NextClock(item.ID.Client))
			}
		}
		s.pending.missing, s.pending.ready = missing, false
		s.pending.tripped, s.pending.scanLeft = true, stop.left
		if stop.scan && len(s.pendingDs.clients) > 0 {
			retryPendingDsUntilStopped(txn)
		}
	}()
	return drain(txn)
}

// retryPendingDsUntilStopped is retryPendingDs for after a drain stopped: a
// retry the apply budget or the allowed roots stop leaves its deletes parked
// and the apply standing.
func retryPendingDsUntilStopped(txn *Transaction) {
	defer func() {
		if r := recover(); r != nil {
			if _, stop := drainStop(r); !stop {
				panic(r)
			}
		}
	}()
	retryPendingDs(txn)
}

// recoveredApplyErr is the error an apply returns for a panic it recovered.
func recoveredApplyErr(r any) error {
	switch r := r.(type) {
	case integrateScanBudgetExceeded:
		return ErrIntegrateScanBudget
	case applyBudgetExceeded:
		return ErrApplyBudget
	case rootNotAllowed:
		return ErrRootNotAllowed
	case budgetCallbackPanic:
		return fmt.Errorf("%w: panic in the %s callback: %v", ErrInvalidUpdate, r.option, r.v)
	}
	return integrationPanicErr(r)
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
			Content: NewContentDeleted(length),
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
			Content: &contentSkip{length: length},
		}, nil
	}

	hasOrigin := info&flagHasOrigin != 0
	hasRightOrigin := info&flagHasRightOrigin != 0
	hasParentSub := info&flagHasParentSub != 0

	var origin, originRight *ID

	if hasOrigin {
		id, err := readID(dec)
		if err != nil {
			return nil, err
		}
		origin = &id
	}

	if hasRightOrigin {
		id, err := readID(dec)
		if err != nil {
			return nil, err
		}
		originRight = &id
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
			if err := refuseRoot(doc, name); err != nil {
				return nil, err
			}
			noteRootName(doc, name)
			parent = doc.getOrCreateType(name)
		} else {
			// Nested type: referenced by container item's ID.
			pid, err := readID(dec)
			if err != nil {
				return nil, err
			}
			parentItem := doc.store.Find(pid)
			if parentItem == nil {
				// Container not integrated yet. EncodeStateAsUpdate sorts groups
				// ascending by client, so a lower-clientID item written into a
				// higher-clientID peer's nested type decodes before its parent.
				// Defer instead of hard-failing (Yjs pendingStructs parity,
				// review finding C-3): record the parent ID and let integration
				// resolve it once the container arrives.
				parentByID = &pid
			} else if ct, ok := parentItem.Content.(*ContentType); ok {
				parent = ct.Type
			} else {
				// Parent resolves to a tombstoned/GC'd container (its ContentType
				// was replaced by a ContentDeleted placeholder). Treat it as a
				// deferred/orphan reference rather than hard-failing: the item
				// ends up with no parent and drops from the visible tree, matching
				// Yjs's parent=nil handling. Aborting the whole update here would
				// discard unrelated structs in the same batch.
				parentByID = &pid
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
		return NewContentDeleted(n), nil

	case wireJSON:
		n, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		if n > uint64(dec.Remaining()) {
			return nil, ErrInvalidUpdate
		}
		// The declared count is charged before a list is made for it; what each
		// value holds is counted off the value its text decoded to.
		tally := doc.tally()
		tally.decode(int(n))
		if n == 0 || !isAnyTag(dec.RemainingBytes()[0]) {
			vals, err := readJSONVals(dec, n)
			if err != nil {
				return nil, err
			}
			tally.nestedAll(vals)
			return NewContentJSON(vals...), nil
		}
		// ygo ≤1.51.0 wrote lib0 Any per value, always starting with a tag
		// 116–127, as does a 116–127-byte JSON text. One encoder writes the
		// whole item, so try JSON text for all values, else Any for all.
		// JSON text (Yjs) wins input valid both ways.
		rem := dec.RemainingBytes()
		sub := encoding.NewDecoder(rem)
		vals, jsonErr := readJSONVals(sub, n)
		if jsonErr == nil {
			for range len(rem) - sub.Remaining() {
				_, _ = dec.ReadUint8()
			}
			tally.nestedAll(vals)
			return NewContentJSON(vals...), nil
		}
		if vals, err = readLegacyAnyVals(dec, n); err != nil {
			return nil, jsonErr
		}
		tally.nestedAll(vals)
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
		doc.tally().value(v)
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
		doc.tally().value(val)
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
		// Every value is counted off the frame before the list is made.
		countAnyRun(doc.tally(), dec, n)
		vals := make([]any, n)
		for i := range vals {
			if vals[i], err = dec.ReadAny(); err != nil {
				return nil, err
			}
		}
		return NewContentAny(vals...), nil

	case wireDoc:
		if doc.wholeOrNothing {
			return nil, errWholeOrNothingContent
		}
		// A guid that is not UTF-8 is refused here, where WithGUID would panic.
		guid, err := dec.ReadVarString()
		if err != nil {
			return nil, err
		}
		countAnyRun(doc.tally(), dec, 1)
		optsAny, err := dec.ReadAny()
		if err != nil {
			return nil, err
		}
		return NewContentDoc(newSubdocFromOpts(guid, optsAny)), nil

	case wireMove:
		if doc.wholeOrNothing {
			return nil, errWholeOrNothingContent
		}
		clientU, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		clock, err := readClock(dec)
		if err != nil {
			return nil, err
		}
		targetLen, err := dec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		target := &ID{Client: ClientID(clientU), Clock: clock}
		return NewContentMove(target, targetLen), nil

	default:
		return nil, unknownTagErr(false, tag)
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

	case 5: // YXmlHook
		hookName, err := dec.ReadVarString()
		if err != nil {
			return nil, err
		}
		h := newYXmlHook(hookName)
		h.doc = doc
		return &h.abstractType, nil

	case 6: // YXmlText
		xt := NewYXmlText()
		xt.doc = doc
		return &xt.abstractType, nil

	default:
		return nil, typeRefError(typeClass)
	}
}

// decodeDeleteSet returns the ranges it read in full, sorted and merged per
// client, also on an error, since Yjs has applied those by then.
func decodeDeleteSet(dec *encoding.Decoder) (DeleteSet, error) {
	ds, _, err := decodeDeleteSetRanges(dec)
	return ds, err
}

// decodeDeleteSetRanges is decodeDeleteSet with ranges, how many ranges it
// read off the frame before the sort and merge, which an apply counts.
func decodeDeleteSetRanges(dec *encoding.Decoder) (ds DeleteSet, ranges int, err error) {
	defer func() {
		for c := range ds.clients {
			ds.sortAndCompact(c)
		}
	}()
	n, err := dec.ReadVarUint()
	if err != nil {
		return newOrderedDeleteSet(), 0, err
	}
	// A first pass on a copy of the cursor counts each client's ranges, so the
	// decode sizes every list once; an entry with a range takes at least four
	// bytes, which bounds the tally, and the counts are of ranges read in full.
	scan, tally := *dec, rangeTally{room: int(min(n, uint64(dec.Remaining()/4)))}
	scanErr := readDeleteSet(&scan, n, tally.count)
	if len(tally.clients) == 0 {
		*dec = scan // the pass read what the decode would, and added nothing
		return newOrderedDeleteSet(), 0, scanErr
	}
	tally.merge()
	ds = tally.deleteSet()
	err = readDeleteSet(dec, n, func(client ClientID, r DeleteRange, _ bool) {
		ds.appendDecoded(client, r, &tally)
	})
	return ds, tally.total(), err
}

// readDeleteSet reads the n client entries of a V1 delete set, calling add for
// each range read in full, with first set for an entry's first range.
func readDeleteSet(dec *encoding.Decoder, n uint64, add func(client ClientID, r DeleteRange, first bool)) error {
	for range n {
		clientU, err := dec.ReadVarUint()
		if err != nil {
			return err
		}
		numRanges, err := dec.ReadVarUint()
		if err != nil {
			return err
		}
		for j := range numRanges {
			clock, err := dec.ReadVarUint()
			if err != nil {
				return err
			}
			length, err := dec.ReadVarUint()
			if err != nil {
				return err
			}
			if _, err := structEnd(clock, length); err != nil {
				return err
			}
			add(ClientID(clientU), DeleteRange{Clock: clock, Len: length}, j == 0)
		}
	}
	return nil
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

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, 2^53-1: a Yjs peer
// holds no clock past it exactly, so no update an honest peer sends has one.
// The decoders refuse a struct, origin, parent or delete-range clock past it,
// because both encodings can carry every clock up to it and V2 cannot carry
// one much past it (its clock columns hold 55 bits).
const maxSafeInteger = 1<<53 - 1

// readClock reads a struct clock, refused past maxSafeInteger.
func readClock(dec *encoding.Decoder) (uint64, error) {
	clock, err := dec.ReadVarUint()
	if err == nil && clock > maxSafeInteger {
		err = ErrInvalidUpdate
	}
	return clock, err
}

// maxClient is the largest client ID a V2 update can carry: V2 writes its
// client column as lib0 VarInts, whose magnitude holds 55 bits. The V1
// decoder refuses a struct, origin or parent client past it, so every struct
// a V1 update brings can be written again as V2. Yjs client IDs fit in 53 bits.
const maxClient = 1<<55 - 1

// readID reads a V1 client and clock: a struct group's start, or an origin,
// right origin or parent reference. It refuses a client past maxClient and a
// clock past maxSafeInteger.
func readID(dec *encoding.Decoder) (ID, error) {
	client, err := dec.ReadVarUint()
	if err != nil {
		return ID{}, err
	}
	if client > maxClient {
		return ID{}, ErrInvalidUpdate
	}
	clock, err := readClock(dec)
	return ID{Client: ClientID(client), Clock: clock}, err
}

// structEnd is the clock after a struct or range of length n at clock,
// refused when the struct would reach a clock past maxSafeInteger.
func structEnd(clock, n uint64) (uint64, error) {
	if clock > maxSafeInteger || n > maxSafeInteger+1-clock {
		return 0, ErrInvalidUpdate
	}
	return clock + n, nil
}

// wrapUpdateErr names err as an invalid update once: an error that already is
// one is returned as it is.
func wrapUpdateErr(err error) error {
	if err == nil || errors.Is(err, ErrInvalidUpdate) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrInvalidUpdate, err)
}

// fmtValToJSON serialises a ContentFormat/ContentEmbed/ContentJSON value as
// JSON text, matching Yjs's JSON.stringify. HTML escaping is off because
// JSON.stringify never escapes <, > or &.
//
// A non-finite number is written as null, as JSON.stringify does; any other
// unencodable value (one that bypassed checkTextValue) panics, as WriteAny does.
func fmtValToJSON(v any) string {
	if v == nil {
		return "null"
	}
	s, err := encodeJSONText(v)
	if err != nil {
		if s, err = encodeJSONText(nullNonFinite(v)); err != nil {
			panic("crdt: value is not JSON-encodable: " + err.Error())
		}
	}
	return s
}

func encodeJSONText(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// nullNonFinite returns v with every NaN/±Inf replaced by nil.
func nullNonFinite(v any) any {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil
		}
	case float32:
		if f := float64(t); math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = nullNonFinite(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = nullNonFinite(e)
		}
		return out
	}
	return v
}

// fmtValFromJSON parses JSON text written by fmtValToJSON or Yjs's
// JSON.stringify, with Yjs's 'undefined' marker as nil. Numbers decode as
// float64, booleans as bool, null as nil.
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

// isAnyTag reports whether b is a lib0 Any type tag.
func isAnyTag(b byte) bool { return b >= 116 && b <= 127 }

// readJSONVals reads n ContentJSON values in Yjs's JSON-text form.
func readJSONVals(dec *encoding.Decoder, n uint64) ([]any, error) {
	vals := make([]any, n)
	for i := range vals {
		js, err := dec.ReadVarString()
		if err != nil {
			return nil, err
		}
		if vals[i], err = fmtValFromJSON(js); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

// readLegacyAnyVals reads n ContentJSON values in ygo ≤1.51.0's lib0 Any
// form, with numbers widened to float64 as the JSON form decodes them.
func readLegacyAnyVals(dec *encoding.Decoder, n uint64) ([]any, error) {
	vals := make([]any, n)
	for i := range vals {
		v, err := dec.ReadAny()
		if err != nil {
			return nil, err
		}
		vals[i] = jsonNumbers(v)
	}
	return vals, nil
}

// jsonNumbers widens lib0 Any's float32 and int64 to float64, recursively.
func jsonNumbers(v any) any {
	switch x := v.(type) {
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case []any:
		for i := range x {
			x[i] = jsonNumbers(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = jsonNumbers(x[k])
		}
	}
	return v
}

// appendTrimmed stores an item without integrating it, dropping the prefix the
// store already has for its client. existingEnd must be at least the item's
// clock. The item is checked and counted as one that integrates
// (noteIntegrated).
func appendTrimmed(txn *Transaction, item *Item, existingEnd uint64) {
	if offset := existingEnd - item.ID.Clock; offset > 0 {
		item.ID.Clock += offset
		item.Content = item.Content.Splice(offset)
	}
	noteIntegrated(txn, item)
	txn.doc.store.Append(item)
}

// errGCContinuation refuses an item whose new clocks follow its client's GC
// struct, which Yjs cannot integrate either.
var errGCContinuation = errors.New("item continues its client's GC struct")

// continuesGC reports whether item, integrated past offset, would take its
// client's GC struct as its left neighbour, which Yjs throws on unless an
// origin of the item is a GC struct.
func continuesGC(store *StructStore, item *Item, offset uint64) bool {
	if offset == 0 {
		return false
	}
	structs := store.clients[item.ID.Client]
	return structs[len(structs)-1].isGCOrphan() && !namesGCStruct(store, item.Origin) && !namesGCStruct(store, item.OriginRight)
}

// integrateBeforeRefusal integrates the structs the update deferred before an
// item it refuses whose dependencies are now present, and drops the rest; the
// caller returns the refusal, so a second one adds nothing.
func integrateBeforeRefusal(txn *Transaction, pending []*Item) {
	_, _ = resolveDeferred(txn, pending)
}

// missingDep is the client Yjs getMissing names for item: that of the first of
// its origin, right origin and parent of another client past what store holds.
func missingDep(store *StructStore, item *Item) (ClientID, bool) {
	c := item.ID.Client
	if o := item.Origin; o != nil && o.Client != c && o.Clock >= store.NextClock(o.Client) {
		return o.Client, true
	}
	if o := item.OriginRight; o != nil && o.Client != c && o.Clock >= store.NextClock(o.Client) {
		return o.Client, true
	}
	if p := item.parentID; p != nil && item.Parent == nil && p.Client != c && p.Clock >= store.NextClock(p.Client) {
		return p.Client, true
	}
	return 0, false
}

// hasMissingDep reports whether missingDep names a client.
func hasMissingDep(store *StructStore, item *Item) bool {
	_, ok := missingDep(store, item)
	return ok
}

// errFutureOwnRef refuses an item that names a struct of its own client past
// that client's clock, which Yjs throws on when it looks the struct up.
var errFutureOwnRef = errors.New("item names a struct of its own client that does not exist")

// namesFutureOwn reports whether item names, as its origin, right origin or
// parent, a clock of its own client at or past next, the client's clock.
func namesFutureOwn(item *Item, next uint64) bool {
	c := item.ID.Client
	if o := item.Origin; o != nil && o.Client == c && o.Clock >= next {
		return true
	}
	if o := item.OriginRight; o != nil && o.Client == c && o.Clock >= next {
		return true
	}
	// Only a struct with no origins names a parent, which Yjs then reads.
	p := item.parentID
	return p != nil && item.Parent == nil && p.Client == c && p.Clock >= next
}

// namesGCStruct reports whether id names a GC struct in store.
func namesGCStruct(store *StructStore, id *ID) bool {
	if id == nil {
		return false
	}
	s := store.Find(*id)
	return s != nil && s.isGCOrphan()
}

// rightIsGC resolves item.Right from its right origin, as Yjs getMissing does,
// and reports whether that struct is a GC struct, which it leaves whole.
func rightIsGC(txn *Transaction, item *Item) bool {
	if item.OriginRight == nil || item.Right != nil {
		return false
	}
	r := txn.doc.store.Find(*item.OriginRight)
	if r == nil {
		return false
	}
	if r.isGCOrphan() {
		return true
	}
	if c := item.OriginRight.Clock; c > r.ID.Clock {
		r = splitItem(txn, r, c-r.ID.Clock)
	}
	item.Right = r
	return false
}

// tryIntegrate attempts to integrate item into the doc store. Returns
// true on success (item is now integrated into the linked list, or
// stored as an orphan when its parent is truly unresolvable). Returns
// false if blocked on a future-clock dependency — the caller should
// leave it in the pending queue — with waitsOn, the client of the first
// unmet one: item's own client past a gap, then its origin, right origin
// and parent. An item that continues its client's GC struct also leaves
// the queue: tryIntegrate returns true and an error.
//
// Used by the inline retry loop to drain pending items that may now
// be integrable. Parallels the normal decode-loop path but with items
// that have already been decoded.
func tryIntegrate(txn *Transaction, item *Item) (ok bool, waitsOn ClientID, err error) {
	store := txn.doc.store

	// Read bounds directly in the retry hot path; missing clients need no
	// call to the content-length clock helper.
	existingEnd := uint64(0)
	if items := store.clients[item.ID.Client]; len(items) > 0 {
		last := items[len(items)-1]
		existingEnd = last.ID.Clock + last.Content.Len()
	}

	// A same-client clock gap is blocked regardless of content length.
	if item.ID.Clock > existingEnd {
		return false, item.ID.Client, nil
	}

	// Yjs looks up what an item names before it checks the item's clocks,
	// waiting on another client's missing struct, else throwing on its own.
	if namesFutureOwn(item, existingEnd) {
		if client, ok := missingDep(store, item); ok {
			return false, client, nil
		}
		return true, 0, wrapUpdateErr(errFutureOwnRef)
	}
	if item.ID.Clock+item.Content.Len() <= existingEnd {
		if client, ok := missingDep(store, item); ok {
			return false, client, nil
		}
		return true, 0, nil
	}

	// From here a drain changes the parked item in place.
	if j := txn.doc.applying; j != nil {
		j.keepParked(item)
	}

	// GC-orphan path (no parent, deleted): store without integration, trimmed
	// to the not-yet-integrated suffix exactly as the decode loops do.
	//
	// The trim matters here and not only there: this item was parked for a
	// same-client clock gap, and by the time the retry runs the store's clock
	// for that client can have advanced INTO (but not through) the item's
	// range. Appending it untrimmed would leave overlapping, out-of-order
	// structs behind, in a store whose Append and state-vector logic assume a
	// contiguous per-client list. The guards above leave clock <= existingEnd
	// here, so the offset is never negative.
	if item.Parent == nil && item.Deleted {
		appendTrimmed(txn, item, existingEnd)
		store.resolvePendingMoves(txn, item.ID.Client)
		return true, 0, nil
	}

	// Try to resolve parent via Origin / OriginRight / ParentSub fallback.
	// As in the first-pass decode, a keyed item that arrived with an origin
	// has no on-wire parentSub and must inherit the key from its origin
	// neighbour here too, or it integrates keyless and is dropped from the
	// parent's itemMap. (#YMap-wire)
	if item.Parent == nil {
		if item.Origin != nil {
			if len(store.clients[item.Origin.Client]) == 0 {
				return false, item.Origin.Client, nil
			}
			if oi := store.Find(*item.Origin); oi != nil {
				item.Parent = oi.Parent
				if item.ParentSub == nil {
					item.ParentSub = oi.ParentSub
				}
			} else if item.Origin.Clock >= store.NextClock(item.Origin.Client) {
				return false, item.Origin.Client, nil // future clock — still blocked
			}
		}
		if item.Parent == nil && item.OriginRight != nil {
			if len(store.clients[item.OriginRight.Client]) == 0 {
				return false, item.OriginRight.Client, nil
			}
			if ori := store.Find(*item.OriginRight); ori != nil {
				// An item whose origin is a GC struct keeps no parent, so it
				// becomes a GC struct below, as in Yjs.
				if item.Origin == nil {
					item.Parent = ori.Parent
					if item.ParentSub == nil {
						item.ParentSub = ori.ParentSub
					}
				}
			} else if item.OriginRight.Clock >= store.NextClock(item.OriginRight.Client) {
				return false, item.OriginRight.Client, nil
			}
		}
		// Parent referenced by container item-ID (review finding C-3): resolve
		// precisely if the container has arrived; if it references a future
		// clock, park (return false) rather than grafting onto an arbitrary map
		// via the ParentSub fallback below.
		if item.Parent == nil && item.parentID != nil {
			if len(store.clients[item.parentID.Client]) == 0 {
				return false, item.parentID.Client, nil
			}
			if pi := store.Find(*item.parentID); pi != nil {
				if ct, ok := pi.Content.(*ContentType); ok {
					item.Parent = ct.Type
				}
			} else if item.parentID.Clock >= store.NextClock(item.parentID.Client) {
				return false, item.parentID.Client, nil
			}
		}
		if item.Parent == nil {
			// A keyed item still unresolved here is a genuine orphan (its
			// container/origin was deleted and GC'd). Yjs drops it on every
			// peer; do NOT graft it onto an arbitrary map by scanning the store,
			// which diverges by integration order (#156). Store it as the GC
			// struct Yjs integrates it as.
			item.markGCOrphan()
			appendTrimmed(txn, item, existingEnd)
			store.resolvePendingMoves(txn, item.ID.Client)
			return true, 0, nil
		}
	}

	// Origin present but referring to a future clock -> still blocked.
	if item.Origin != nil {
		items := store.clients[item.Origin.Client]
		if len(items) == 0 {
			return false, item.Origin.Client, nil
		}
		last := items[len(items)-1]
		if item.Origin.Clock >= last.ID.Clock+last.Content.Len() {
			return false, item.Origin.Client, nil
		}
	}

	// OriginRight referring to a future clock -> still blocked. Yjs's getMissing
	// blocks integration on origin, rightOrigin, OR parent; ygo only checked
	// OriginRight inside the Parent==nil branch above. A root type's parent is
	// resolved by name, so that branch is skipped and an item whose right origin
	// is a not-yet-integrated client would otherwise integrate at the wrong
	// position (permanent divergence, review finding C-2). itemFutureDep already
	// reports OriginRight as the missing dependency, so the item parks here and
	// retries once that client's clock advances.
	if item.OriginRight != nil {
		items := store.clients[item.OriginRight.Client]
		if len(items) == 0 {
			return false, item.OriginRight.Client, nil
		}
		last := items[len(items)-1]
		if item.OriginRight.Clock >= last.ID.Clock+last.Content.Len() {
			return false, item.OriginRight.Client, nil
		}
	}

	offset := existingEnd - item.ID.Clock
	if continuesGC(store, item, offset) {
		return true, 0, wrapUpdateErr(errGCContinuation)
	}

	// Resolve left neighbor for integrate().
	if item.Origin != nil {
		item.Left = store.getItemCleanEnd(txn, item.Origin.Client, item.Origin.Clock)
	}
	// Yjs resolves an item's neighbours and parent when it integrates the
	// item. A left or right neighbour that is a GC struct (also when the item
	// found a parent through its right origin), or a parent type the GC
	// collected while the item was parked, makes it a GC struct, as an
	// unresolvable parent does above.
	if (item.Left != nil && item.Left.isGCOrphan()) || typeCollected(item.Parent) || rightIsGC(txn, item) {
		item.Left = nil
		item.markGCOrphan()
		appendTrimmed(txn, item, existingEnd)
		store.resolvePendingMoves(txn, item.ID.Client)
		return true, 0, nil
	}

	// A parked range may now overlap the store; integrate only its new suffix.
	item.integrate(txn, offset)
	return true, 0, nil
}

// typeCollected reports whether the GC collected t: its holder item no longer
// holds it.
func typeCollected(t *abstractType) bool {
	if t == nil || t.item == nil {
		return false
	}
	ct, ok := t.item.Content.(*ContentType)
	return !ok || ct.Type != t
}

// itemFutureDep reports whether item is blocked on a future-clock dependency
// (one whose referenced clock has not yet been integrated into the store).
// Returns (missingClient, storeClockAtParkTime, true) if yes; otherwise
// (0, 0, false) indicating the item's parent is truly unresolvable (e.g.
// origin references a GC placeholder whose parent info was lost).
func itemFutureDep(item *Item, store *StructStore) (ClientID, uint64, bool) {
	// A gap of its own client, then another client's missing struct, come
	// first, in tryIntegrate's order, which is Yjs getMissing's.
	if next := store.NextClock(item.ID.Client); item.ID.Clock > next {
		return item.ID.Client, next, true
	}
	if client, ok := missingDep(store, item); ok {
		return client, store.NextClock(client), true
	}
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
