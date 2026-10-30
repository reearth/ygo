package crdt

import (
	"cmp"
	"encoding/binary"
	"math"
	"slices"
	"sort"

	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// DeleteRange is a contiguous range of deleted clocks for a single client.
type DeleteRange struct {
	Clock uint64
	Len   uint64
}

// DeleteSet tracks which Items have been deleted, stored as sorted, non-overlapping
// ranges per client. This compact representation is what travels on the wire.
type DeleteSet struct {
	clients map[ClientID][]DeleteRange
	// order, when non-nil, lists clients in first-add order (Yjs DeleteSet's
	// Map order; for a decoded set, encoded order), which deletes are applied
	// and replayed in. Sets built from the store leave it nil.
	order []ClientID
}

func newDeleteSet() DeleteSet {
	return DeleteSet{clients: make(map[ClientID][]DeleteRange)}
}

// newOrderedDeleteSet returns a set that records client first-add order.
func newOrderedDeleteSet() DeleteSet {
	return DeleteSet{clients: make(map[ClientID][]DeleteRange), order: []ClientID{}}
}

// add records that item (client, clock) with the given length has been deleted.
// Adjacent ranges are merged eagerly to keep the set compact.
func (ds *DeleteSet) add(id ID, length int) {
	ranges, seen := ds.clients[id.Client]
	if !seen && ds.order != nil {
		ds.order = append(ds.order, id.Client)
	}
	if len(ranges) > 0 {
		last := &ranges[len(ranges)-1]
		if last.Clock+last.Len == id.Clock {
			last.Len += uint64(length)
			ds.clients[id.Client] = ranges
			return
		}
	}
	ds.clients[id.Client] = append(ranges, DeleteRange{
		Clock: id.Clock,
		Len:   uint64(length),
	})
}

// IsDeleted reports whether the item at the given ID has been marked deleted.
func (ds *DeleteSet) IsDeleted(id ID) bool {
	for _, r := range ds.clients[id.Client] {
		if r.Clock <= id.Clock && id.Clock < r.Clock+r.Len {
			return true
		}
	}
	return false
}

// Merge incorporates all ranges from other into ds, appending other's new
// clients after ds's (Yjs mergeDeleteSets order).
func (ds *DeleteSet) Merge(other DeleteSet) {
	if ds.order != nil {
		for _, client := range other.orderedClients() {
			if _, ok := ds.clients[client]; !ok {
				ds.order = append(ds.order, client)
			}
		}
	}
	for client, ranges := range other.clients {
		ds.clients[client] = append(ds.clients[client], ranges...)
	}
	// Re-sort and compact all affected clients.
	for client := range other.clients {
		ds.sortAndCompact(client)
	}
}

func (ds *DeleteSet) sortAndCompact(client ClientID) {
	ranges := ds.clients[client]
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Clock < ranges[j].Clock
	})
	compacted := ranges[:0]
	for _, r := range ranges {
		if len(compacted) > 0 {
			last := &compacted[len(compacted)-1]
			if last.Clock+last.Len >= r.Clock {
				end := r.Clock + r.Len
				if end > last.Clock+last.Len {
					last.Len = end - last.Clock
				}
				continue
			}
		}
		compacted = append(compacted, r)
	}
	ds.clients[client] = compacted
}

// Clients returns the client IDs that have at least one deleted range in ds.
func (ds *DeleteSet) Clients() []ClientID {
	out := make([]ClientID, 0, len(ds.clients))
	for c := range ds.clients {
		out = append(out, c)
	}
	return out
}

// orderedClients returns ds's clients in first-add (for a decoded set,
// encoded) order, then any unordered ones ascending. order holds each client
// of ds.clients at most once.
func (ds *DeleteSet) orderedClients() []ClientID {
	out := make([]ClientID, len(ds.order), len(ds.clients))
	copy(out, ds.order)
	if len(out) == len(ds.clients) {
		return out
	}
	listed := make(map[ClientID]struct{}, len(out))
	for _, c := range out {
		listed[c] = struct{}{}
	}
	var rest []ClientID
	for c := range ds.clients {
		if _, ok := listed[c]; !ok {
			rest = append(rest, c)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

// retryPendingDs reapplies the store's parked deletions in the order Yjs
// re-reads its encoded pending delete set: clients descending, ranges sorted.
// It skips a client whose next clock has not passed its lowest parked range,
// since none of that client's ranges can apply yet.
func retryPendingDs(txn *Transaction) {
	store := txn.doc.store
	var ready DeleteSet
	for client, ranges := range store.pendingDs.clients {
		if len(ranges) > 0 && store.NextClock(client) > ranges[0].Clock {
			if ready.clients == nil {
				ready = newDeleteSet()
			}
			ready.clients[client] = ranges
		}
	}
	if ready.clients == nil {
		return
	}
	kept := store.pendingDs
	store.pendingDs = newDeleteSet()
	ready.order = make([]ClientID, 0, len(ready.clients))
	for c := range ready.clients {
		ready.order = append(ready.order, c)
		ready.sortAndCompact(c)
	}
	slices.SortFunc(ready.order, func(a, b ClientID) int { return cmp.Compare(b, a) })
	still := ready.applyToPartial(txn)
	for client, ranges := range kept.clients {
		if _, retried := ready.clients[client]; !retried && len(ranges) > 0 {
			still.clients[client] = ranges
		}
	}
	store.pendingDs = still
}

// applyToPartial applies delete-set entries whose target items are
// integrated, and returns a new DeleteSet containing entries whose
// target items are absent (or the uncovered suffix of a range whose
// earlier clocks are integrated but later clocks are not). The caller
// should merge the returned set into StructStore.pendingDs and retry
// it on subsequent applies.
//
// This is the pending-aware variant of applyTo, used by applyV1Txn
// and applyV2Txn to defer deletes that target not-yet-integrated items.
//
// invariant: store.clients[client] is contiguous from clock 0.
// If a future change relaxes contiguity, the applied-prefix math
// may under-park ranges spanning a gap.
func (ds *DeleteSet) applyToPartial(txn *Transaction) DeleteSet {
	unresolvable := newDeleteSet()
	for _, client := range ds.orderedClients() {
		ranges := ds.clients[client]
		items := txn.doc.store.clients[client]
		if len(items) == 0 {
			// No items for this client — entire set of ranges is unresolvable.
			for _, r := range ranges {
				if r.Len > 0 {
					unresolvable.clients[client] = append(unresolvable.clients[client], r)
				}
			}
			continue
		}
		for _, r := range ranges {
			// A zero-length range deletes nothing (Yjs readAndApplyDeleteSet).
			// Without this skip the loop below tombstones the item containing
			// r.Clock, since no boundary split isolates an empty range.
			if r.Len == 0 {
				continue
			}
			// Split at the range boundaries so each overlapping item lies
			// entirely inside [r.Clock, r.Clock+r.Len). Pre-#72 we deleted
			// overlapping items whole, which wiped content outside the range
			// when the item was a locally-squashed run that the sender saw as
			// shorter. getItemCleanStart is a no-op when no item contains the
			// target clock (boundary already clean or past the store).
			//
			// Mirrors Yjs JS iterateDeletedStructs and yrs Update::integrate
			// which both pre-split at boundaries before tombstoning.
			if r.Len > 0 {
				txn.doc.store.getItemCleanStart(txn, ID{Client: client, Clock: r.Clock})
				txn.doc.store.getItemCleanStart(txn, ID{Client: client, Clock: r.Clock + r.Len})
				// Splits may have inserted new items; refresh the slice.
				items = txn.doc.store.clients[client]
			}

			// Binary search: first item whose end > r.Clock.
			lo := sort.Search(len(items), func(i int) bool {
				return items[i].ID.Clock+uint64(items[i].Content.Len()) > r.Clock
			})
			applied := uint64(0)
			for i := lo; i < len(items); i++ {
				item := items[i]
				if item.ID.Clock >= r.Clock+r.Len {
					break
				}
				// After our boundary splits, item is entirely inside the range.
				end := r.Clock + r.Len
				itemEnd := item.ID.Clock + uint64(item.Content.Len())
				if itemEnd < end {
					end = itemEnd
				}
				item.delete(txn)
				// Search-marker invalidation for the remote delete-apply path
				// (the v1.31.6 stale-cache class, #181). item.delete clears only
				// when !txn.Local, and Local is a caller-writable field, so do
				// not rely on it here. The rendered index of the tombstoned item is not
				// tracked by this delete-set walk, so a precise
				// updateMarkerChanges(index, -len) is not available — we clear all
				// markers, which is always safe (the next lookup repopulates via a
				// cold walk). Without this, still-live markers after the tombstone
				// would keep an index that is now too high and mis-resolve the next
				// local positioned insert (#160).
				if item.Parent != nil && item.Content.IsCountable() {
					item.Parent.clearMarkers()
				}
				applied = end - r.Clock
			}
			// Park the uncovered suffix of the range, if any.
			if applied < r.Len {
				unresolvable.clients[client] = append(unresolvable.clients[client], DeleteRange{
					Clock: r.Clock + applied,
					Len:   r.Len - applied,
				})
			}
		}
	}
	return unresolvable
}

// checkParkedDeletes refuses, before the apply changes anything, an update
// whose delete set would leave more parked ranges than WithMaxPendingItems
// allows, so a refused update leaves the document as it was. A range takes at
// least two bytes, so a short update is accepted on its length alone, and a
// longer one is first bounded by one walk (frameStructs, parkingRangesBound)
// before the exact count.
func checkParkedDeletes(doc *Doc, b *pendingBudget) error {
	limit := doc.maxPendingItemsLimit()
	pending := doc.store.pendingDs
	before := 0
	for _, ranges := range pending.clients {
		before += len(ranges)
	}
	room := max(limit-before, 0)
	if len(b.update)/2 <= room {
		return nil
	}
	s := b.scanner()
	if _, err := frameStructs(s); err != nil {
		return wrapUpdateErr(err)
	}
	if n, err := parkingRangesBound(s, b.initial); err != nil {
		return wrapUpdateErr(err)
	} else if n <= room {
		return nil
	}
	ds, err := deleteSetOf(b.scanner(), b.v2)
	if err != nil {
		return wrapUpdateErr(err)
	}
	if n := parkingRanges(ds, b.initial); n <= room {
		return nil
	}
	clocks, err := structClocksOf(b)
	if err != nil {
		return wrapUpdateErr(err)
	}
	after := parkedDeleteRangesAfter(stillParked(pending, b.initial, clocks), parkedPartsOf(ds, clocks))
	if after <= limit || after <= before {
		return nil
	}
	return ErrInvalidUpdate
}

// stillParked is what the retry after the update's structs leaves of pending:
// the ranges of each client whose clock the structs moved, cut at its clock in
// clocks.
func stillParked(pending DeleteSet, initial, clocks StateVector) DeleteSet {
	still := DeleteSet{clients: make(map[ClientID][]DeleteRange, len(pending.clients))}
	for client, ranges := range pending.clients {
		next := clocks.Clock(client)
		if next == initial.Clock(client) {
			still.clients[client] = ranges
			continue
		}
		var kept []DeleteRange
		for _, r := range ranges {
			if part, parks := parkedPart(r, next); parks {
				kept = append(kept, part)
			}
		}
		if len(kept) > 0 {
			still.clients[client] = kept
		}
	}
	return still
}

// deleteSetOf is the delete set of the update s scans, a V1 one or a V2 one,
// read past its structs without keeping anything of them.
func deleteSetOf(s *pendingScanner, v2 bool) (DeleteSet, error) {
	if _, err := frameStructs(s); err != nil {
		return DeleteSet{}, err
	}
	if v2 {
		return decodeDeleteSetV2(s.v2)
	}
	return decodeDeleteSet(s.rest)
}

// frameStructs reads the update s scans past its struct section, so that s
// stands at its delete set, and is how many structs the section holds, skips
// included. It reads only what it takes to find each struct's end, and checks
// nothing more: a V2 update's group headers and the rest content of its
// structs (v2StructCount), and of a V1 update each struct's info, origins,
// parent and content, with strings and values stepped over by their length,
// unread (frameV1Structs).
func frameStructs(s *pendingScanner) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.v2 != nil {
		n, ok := v2StructCount(s.v2, true)
		if !ok {
			return 0, ErrInvalidUpdate
		}
		return n, nil
	}
	buf := s.rest.RemainingBytes()
	n, end, err := frameV1Structs(buf)
	if err != nil {
		return 0, err
	}
	*s.rest = *encoding.NewDecoder(buf[end:])
	return n, nil
}

// frameV1Structs steps over the struct section at the start of buf, a V1
// update, as decodeItem and decodeContent read it, without decoding its
// strings or values, and is how many structs it holds and where it ends. It
// works on the bytes directly, a varint at a time. JSON content keeps the
// pending scanner's reading, since whether its values are JSON or legacy Any
// decides where they end.
func frameV1Structs(buf []byte) (structs, end int, err error) {
	p := 0
	bad := false
	next := func() uint64 {
		v, n := binary.Uvarint(buf[p:])
		if n <= 0 {
			bad = true
			return 0
		}
		p += n
		return v
	}
	skipBytes := func() {
		l := next()
		if bad || l > uint64(len(buf)-p) {
			bad = true
			return
		}
		p += int(l)
	}
	skipAny := func() {
		n, err := anycodec.Skip(buf[p:])
		if err != nil {
			bad = true
			return
		}
		p += n
	}
	clients := next()
	if bad || clients > maxV2Items {
		return 0, 0, ErrInvalidUpdate
	}
	var total uint64
	for range clients {
		n := next()
		total += n
		if bad || total > maxV2Items {
			return 0, 0, ErrInvalidUpdate
		}
		next() // client
		next() // clock
		for range n {
			if bad || p >= len(buf) {
				return 0, 0, ErrInvalidUpdate
			}
			info := buf[p]
			p++
			tag := info & 0x1f
			if tag == 0 || tag == 10 {
				next()
				continue
			}
			if info&flagHasOrigin != 0 {
				next()
				next()
			}
			if info&flagHasRightOrigin != 0 {
				next()
				next()
			}
			if info&(flagHasOrigin|flagHasRightOrigin) == 0 {
				if bad || p >= len(buf) {
					return 0, 0, ErrInvalidUpdate
				}
				named := buf[p] == 1
				p++
				if named {
					skipBytes()
				} else {
					next()
					next()
				}
				if info&flagHasParentSub != 0 {
					skipBytes()
				}
			}
			if bad {
				return 0, 0, ErrInvalidUpdate
			}
			switch tag {
			case wireDeleted:
				next()
			case wireJSON:
				d := encoding.NewDecoder(buf[p:])
				js := pendingScanner{rest: d}
				js.content(tag)
				if js.err != nil {
					return 0, 0, js.err
				}
				p = len(buf) - d.Remaining()
			case wireBinary, wireString, wireEmbed:
				skipBytes()
			case wireFormat:
				skipBytes()
				skipBytes()
			case wireType:
				if p >= len(buf) {
					return 0, 0, ErrInvalidUpdate
				}
				ref := buf[p]
				p++
				if ref == 3 || ref == 5 {
					skipBytes()
				}
			case wireAny:
				values := next()
				if bad || values > uint64(len(buf)-p) {
					return 0, 0, ErrInvalidUpdate
				}
				for range values {
					if skipAny(); bad {
						break
					}
				}
			case wireDoc:
				skipBytes()
				skipAny()
			case wireMove:
				next()
				next()
				next()
			default:
				return 0, 0, ErrInvalidUpdate
			}
		}
	}
	if bad {
		return 0, 0, ErrInvalidUpdate
	}
	return int(total), p, nil
}

// parkingRangesBound reads the delete set s stands at, keeping nothing of it,
// and is how many of its ranges would park some part on a store whose state
// vector is sv: a bound on what the update parks (parkingRanges), since its
// structs only move the store's clocks past sv and the decoder's merge of
// ranges only joins them. It allocates nothing.
func parkingRangesBound(s *pendingScanner, sv StateVector) (int, error) {
	clients := s.uint()
	if s.err != nil {
		return 0, s.err
	}
	if clients > maxV2Items {
		return 0, ErrInvalidUpdate
	}
	n := 0
	for range clients {
		var clock, length uint64
		var err error
		if s.v2 != nil {
			s.v2.resetDsCurVal()
		}
		client := ClientID(s.uint())
		ranges := s.uint()
		if s.err != nil {
			return 0, s.err
		}
		if ranges > maxV2Items {
			return 0, ErrInvalidUpdate
		}
		next := sv.Clock(client)
		for range ranges {
			if s.v2 != nil {
				if clock, err = s.v2.readDsClock(); err == nil {
					length, err = s.v2.readDsLen()
				}
			} else {
				clock, length = s.uint(), s.uint()
				err = s.err
			}
			if err != nil {
				return 0, err
			}
			if _, parks := parkedPart(DeleteRange{Clock: clock, Len: length}, next); parks {
				n++
			}
		}
	}
	return n, nil
}

// structClocksOf is the store's state vector once the structs of b's update
// are in, resolved as the pending budget resolves them; items an earlier
// update parked are left out, so the count it feeds can only be high.
func structClocksOf(b *pendingBudget) (StateVector, error) {
	if err := b.knowSuperseded(); err != nil {
		return nil, err
	}
	known := make(StateVector, len(b.initial))
	for client, clock := range b.initial {
		known[client] = clock
	}
	sizing := pendingCollector{}
	if _, err := scanPendingGroupsBounded(b.scanner(), known, math.MaxInt, &sizing, nil, nil, b.superseded); err != nil {
		return nil, err
	}
	if sizing.heads == 0 {
		return known, nil
	}
	collect := pendingCollector{fill: true, groups: make([]pendingGroup, 0, sizing.heads), sum: make([]byte, 0, sizing.size)}
	if _, err := scanPendingGroupsBounded(b.scanner(), known, math.MaxInt, &collect, nil, nil, b.superseded); err != nil {
		return nil, err
	}
	if len(collect.groups) > 0 {
		resolvePendingGroups(collect.groups, collect.sum, known)
	}
	return known, nil
}

// parkedPartsOf is what applyToPartial parks of ds on a store whose state
// vector is clocks.
func parkedPartsOf(ds DeleteSet, clocks StateVector) DeleteSet {
	parked := newDeleteSet()
	for client, ranges := range ds.clients {
		next := clocks.Clock(client)
		for _, r := range ranges {
			if part, parks := parkedPart(r, next); parks {
				parked.clients[client] = append(parked.clients[client], part)
			}
		}
	}
	return parked
}

// parkingRanges is how many ranges of ds would park some part on a store whose
// state vector is sv. The update's structs can only move the store's clocks
// past sv, so no more of them park once they are in.
func parkingRanges(ds DeleteSet, sv StateVector) int {
	n := 0
	for client, ranges := range ds.clients {
		next := sv.Clock(client)
		for _, r := range ranges {
			if _, parks := parkedPart(r, next); parks {
				n++
			}
		}
	}
	return n
}

// parkedPart is the part of r that applyToPartial parks on a store holding r's
// client up to next, and whether any of it parks.
func parkedPart(r DeleteRange, next uint64) (DeleteRange, bool) {
	end := r.Clock + r.Len
	switch {
	case r.Len == 0:
		return DeleteRange{}, false
	case next == 0, end < r.Clock:
		return r, true
	case end <= next:
		return DeleteRange{}, false
	}
	start := max(r.Clock, next)
	return DeleteRange{Clock: start, Len: end - start}, true
}

// parkedDeleteRangesAfter is how many delete ranges pending would hold once ds
// is merged into it: Merge sorts each client's ranges and joins those that
// overlap or touch, so ranges ds names again, or that fill the gap between two
// parked ranges, add nothing or take ranges away.
func parkedDeleteRangesAfter(pending, ds DeleteSet) int {
	n := 0
	for client, ranges := range pending.clients {
		if _, merged := ds.clients[client]; !merged {
			n += len(ranges)
		}
	}
	for client, add := range ds.clients {
		n += mergedRangeCount(pending.clients[client], add)
	}
	return n
}

// mergedRangeCount is how many ranges sortAndCompact leaves of have and add
// together. Both are sorted here, on copies, so the count costs what they hold
// and changes neither.
func mergedRangeCount(have, add []DeleteRange) int {
	byClock := func(a, b DeleteRange) int { return cmp.Compare(a.Clock, b.Clock) }
	have, add = slices.Clone(have), slices.Clone(add)
	slices.SortFunc(have, byClock)
	slices.SortFunc(add, byClock)
	count, end := 0, uint64(0)
	for i, j := 0, 0; i < len(have) || j < len(add); {
		var r DeleteRange
		if j == len(add) || (i < len(have) && have[i].Clock <= add[j].Clock) {
			r = have[i]
			i++
		} else {
			r = add[j]
			j++
		}
		if count > 0 && end >= r.Clock {
			end = max(end, r.Clock+r.Len)
			continue
		}
		count++
		end = r.Clock + r.Len
	}
	return count
}
