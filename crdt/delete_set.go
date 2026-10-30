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

// retryPendingDs reapplies the parked deletions that can now apply, in the
// order Yjs re-reads its pending delete set.
func retryPendingDs(txn *Transaction) {
	store := txn.doc.store
	ready := store.takeReadyDeletes()
	if ready.clients == nil {
		return
	}
	slices.SortFunc(ready.order, func(a, b ClientID) int { return cmp.Compare(b, a) })
	store.parkDeletes(ready.applyToPartial(txn))
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

// checkParkedDeletes refuses, before anything changes, an update whose delete
// set would grow the parked ranges past WithMaxPendingItems.
func checkParkedDeletes(doc *Doc, b *pendingBudget) error {
	limit := doc.maxPendingItemsLimit()
	pending := doc.store.pendingDs
	before := doc.store.pendingDsRanges
	room := max(limit-before, 0)
	// A delete range takes at least 2 bytes, a clock and a length, so an update
	// of at most twice the room cannot park past it.
	if len(b.update)/2 <= room {
		return nil
	}
	if err := b.walkGroups(); err != nil {
		return wrapUpdateErr(err)
	}
	if n, err := parkingRangesBound(b.deleteSetScanner(), b.initial); err != nil {
		return wrapUpdateErr(err)
	} else if n <= room {
		return nil
	}
	ds, err := deleteSetOf(b)
	if err != nil {
		return wrapUpdateErr(err)
	}
	clocks, err := structClocksOf(b)
	if err != nil {
		return wrapUpdateErr(err)
	}
	after := parkedDeleteRangesAfter(pending, before, b.initial, clocks, parkedPartsOf(ds, clocks))
	if after <= limit || after <= before {
		return nil
	}
	return ErrInvalidUpdate
}

// deleteSetOf reads the delete set of b's V1 or V2 update, where the walk of
// its structs found it.
func deleteSetOf(b *pendingBudget) (DeleteSet, error) {
	if err := b.walkGroups(); err != nil {
		return DeleteSet{}, err
	}
	s := b.deleteSetScanner()
	if b.v2 {
		return decodeDeleteSetV2(s.v2)
	}
	return decodeDeleteSet(s.rest)
}

// parkingRangesBound counts, without allocating, the ranges of the delete set s
// stands at that would park on a store at sv, an upper bound on what the update
// parks.
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

// structClocksOf is the store's state vector once b's update structs are in,
// leaving out earlier parked items so the count it feeds can only be high.
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

// parkedPartsOf is what applyToPartial parks of ds on a store at clocks.
func parkedPartsOf(ds DeleteSet, clocks StateVector) DeleteSet {
	parked := newDeleteSet()
	for client, ranges := range ds.clients {
		if parts := cutParked(ranges, clocks.Clock(client)); len(parts) > 0 {
			parked.clients[client] = parts
		}
	}
	return parked
}

// parkedPart is the part of r that applyToPartial parks when r's client is held
// up to next.
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

// parkedDeleteRangesAfter is how many ranges pending holds after an update
// moves the clocks from initial to clocks and parks parked, recounting only the
// clients it touches.
func parkedDeleteRangesAfter(pending DeleteSet, before int, initial, clocks StateVector, parked DeleteSet) int {
	after := before
	count := func(client ClientID) {
		have := pending.clients[client]
		if next := clocks.Clock(client); next != initial.Clock(client) {
			have = cutParked(have, next)
		}
		after += mergedRangeCount(have, parked.clients[client]) - len(pending.clients[client])
	}
	for client := range parked.clients {
		count(client)
	}
	for client, clock := range clocks {
		if _, counted := parked.clients[client]; !counted && clock != initial.Clock(client) && len(pending.clients[client]) > 0 {
			count(client)
		}
	}
	return after
}

// cutParked is what the retry leaves of a client's parked ranges once the store
// holds it up to next.
func cutParked(ranges []DeleteRange, next uint64) []DeleteRange {
	var kept []DeleteRange
	for _, r := range ranges {
		if part, parks := parkedPart(r, next); parks {
			kept = append(kept, part)
		}
	}
	return kept
}

// mergedRangeCount is how many ranges sortAndCompact leaves of have and add,
// computed on copies so neither changes.
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
