package crdt

import (
	"cmp"
	"math"
	"slices"
	"sort"
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

// retryPendingDs reapplies the store's parked deletions whose structs have
// arrived since they were parked, in the order Yjs re-reads its encoded
// pending delete set: clients descending, ranges sorted.
//
// A client's ranges are retried only once the store's next clock for it has
// passed its lowest parked range. A retry tries every range below that clock
// and parks what lies at or past it, so until the clock moves none of the
// client's ranges can apply, and an apply that brings nothing for a client
// costs nothing for its parked ranges, however many there are. Without the
// gate every apply, in a document holding a client's ranges nothing resolves,
// sorted and copied all of them: with 100,000 parked ranges one keystroke
// allocated about 1.6 MB.
func retryPendingDs(txn *Transaction) {
	store := txn.doc.store
	var ready DeleteSet
	for client, ranges := range store.pendingDs.clients {
		if len(ranges) > 0 && store.NextClock(client) > lowestClock(ranges) {
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

// lowestClock is the lowest clock the ranges name. They are sorted wherever
// they come from a merge or a retry, so this reads the first; it walks them
// rather than trusting that, since the cost is the client's own ranges once
// per apply and a wrong answer would hold a range back for good.
func lowestClock(ranges []DeleteRange) uint64 {
	lowest := ranges[0].Clock
	for _, r := range ranges[1:] {
		lowest = min(lowest, r.Clock)
	}
	return lowest
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

// checkParkedDeletes refuses a remote update, V1 or V2, when its delete set
// would grow the parked delete ranges past the cap the pending queue has for
// parked items (WithMaxPendingItems): the same ErrInvalidUpdate that an update
// parking too many items is refused with. It runs before the apply changes
// anything, so a refused update leaves the document as it was: no struct is
// integrated, no delete applied and no observer told. b is the apply's pending
// budget, which holds the update and the store's state vector as the apply
// begins.
//
// The ranges an update parks are the parts of its delete set past the store's
// next clock for their client once the update's structs are in
// (applyToPartial). The count is taken after the merge, and an update that
// adds no range is never refused: a delete set a peer sends again, as its sync
// does after a reconnect, or ranges that join parked ones, pass even on a
// document that already holds more than the cap because it loaded a stored
// state with the cap lifted (SetMaxPendingItems).
//
// It costs what the update can do. Each range of a delete set takes two bytes
// of the update at least, its clock and its length, so an update too short to
// carry more ranges than the cap has room for costs a comparison: a keystroke
// on a document below the cap is never read twice. A longer one is read past
// its structs for its delete set, and only when more of its ranges reach past
// the store than the cap has room for are the clocks its structs leave
// resolved (structClocksOf).
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
	ds, err := deleteSetOf(b.scanner(), b.v2)
	if err != nil {
		return wrapUpdateErr(err)
	}
	if n := parkingRanges(ds, b.initial); n == 0 || n <= room {
		return nil
	}
	clocks, err := structClocksOf(b)
	if err != nil {
		return wrapUpdateErr(err)
	}
	after := parkedDeleteRangesAfter(pending, parkedPartsOf(ds, clocks))
	if after <= limit || after <= before {
		return nil
	}
	return wrapUpdateErr(ErrInvalidUpdate)
}

// deleteSetOf is the delete set of the update s scans, a V1 one or a V2 one,
// read past its structs without keeping anything of them.
func deleteSetOf(s *pendingScanner, v2 bool) (DeleteSet, error) {
	if s.err != nil {
		return DeleteSet{}, s.err
	}
	clients := s.uint()
	if clients > maxV2Items {
		return DeleteSet{}, ErrInvalidUpdate
	}
	var total uint64
	for i := uint64(0); i < clients && s.err == nil; i++ {
		n := s.uint()
		total += n
		if total > maxV2Items {
			return DeleteSet{}, ErrInvalidUpdate
		}
		s.client()
		s.uint()
		for j := uint64(0); j < n && s.err == nil; j++ {
			s.item()
		}
	}
	if s.err != nil {
		return DeleteSet{}, s.err
	}
	if v2 {
		return decodeDeleteSetV2(s.v2)
	}
	return decodeDeleteSet(s.rest)
}

// structClocksOf is the store's state vector once the structs of b's update
// are in. The clocks are found as the pending budget finds them
// (scanPendingGroupsBounded, resolvePendingGroups): a struct is in once its
// clock follows its client's and its origin, right origin and parent are in,
// from the store or from the update itself in any order, and the groups the
// decoder drops count for nothing. Items an earlier update parked do not
// count: the drain that can integrate them runs after the update's delete set
// is parked.
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

// parkedPart is the part of r, a delete range of a client the store holds up
// to next, that applyToPartial parks, and whether it parks any: nothing of a
// range of length zero, every other range as it is when the store holds
// nothing of the client, a range whose end wraps whole, and otherwise what
// lies at or past next.
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
