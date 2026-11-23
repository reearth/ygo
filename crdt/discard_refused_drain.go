package crdt

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"sort"
)

// SetDiscardRefusedDrain (off by default) changes what a document that
// applies remote updates whole or nothing (WithWholeOrNothingApplies, or an
// apply check) does with a drain of its pending queue that it rolls back
// (drainParked): the apply check refused what the drain landed
// (WithApplyCheck), or the drain's scans passed the scan bound
// (WithIntegrateScanBudget), or it passed the apply budget or wrote under a
// root the allowed roots leave out (WithApplyBudget, WithAllowedRoots). Off,
// the drain is rolled back and the backlog is parked again whole, so the next
// update that fills the same gap runs into the same refusal. On, the drain is
// rolled back and everything the fill released is discarded instead: every
// parked struct the store now holds the dependencies of, or that another such
// struct supplies - what a drain run to its end would have integrated, read
// from the clocks alone - and every pending delete range, or part of one,
// below the clocks the store and those structs then reach. What the fill did
// not release stays parked.
//
// The update that filled the gap stands, as it does with the option off, and
// the apply returns a *DrainDiscardedError that says what was discarded and
// the origins of the applies that parked it, which the document records at
// each commit that parks something while the option is on. A document that
// does not apply remote updates whole or nothing rolls no drain back, and the
// option changes nothing for it. Turning it off forgets the origins of parked
// deletions it recorded, and ForgetParkedOrigin forgets one origin.
func (d *Doc) SetDiscardRefusedDrain(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.discardRefusedDrain = on
	if !on {
		d.parkedDsBy, d.parkedDsByRanges = nil, 0
	}
}

// ForgetParkedOrigin drops origin from what the document keeps of the apply
// that parked each struct and delete range - on a document with a budget, a
// roots list or SetDiscardRefusedDrain - so the document holds no reference
// to origin and a later DrainDiscardedError does not name it. What origin
// parked stays parked, and a struct's roots list stays with it
// (checkParkedRoot). A server calls it once the sender whose applies carried
// origin has gone: the document would otherwise keep that sender, and all it
// holds, for as long as what it parked stays parked, and a sender that has
// gone has nothing left to close. A nil origin, or one Go cannot compare, is
// not matched. It costs a pass over the record, which the queue bounds.
func (d *Doc) ForgetParkedOrigin(origin any) {
	if origin == nil || !reflect.ValueOf(origin).Comparable() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for it, from := range d.parkedBy {
		switch {
		case from.origin != origin:
		case len(from.roots) == 0:
			delete(d.parkedBy, it)
		default:
			from.origin = forgottenOrigin
			d.parkedBy[it] = from
		}
	}
	shrinkParkedBy(d)
	kept := d.parkedDsBy[:0]
	for _, by := range d.parkedDsBy {
		if by.origin == origin {
			d.parkedDsByRanges -= rangeCount(by.ds)
			continue
		}
		kept = append(kept, by)
	}
	clear(d.parkedDsBy[len(kept):])
	d.parkedDsBy = shrinkParkedDeletes(kept)
}

// forgottenOrigin stands, in what the document keeps of the apply that parked
// a struct, for an origin ForgetParkedOrigin dropped where the struct's roots
// list stays: no apply carries it (dropOutsideRoots) and no discard names it.
var forgottenOrigin any = &struct{ forgotten bool }{}

// DrainDiscardedError is the error of a remote apply whose update stands and
// whose drain of the pending queue was rolled back and what its fill released
// discarded (SetDiscardRefusedDrain). It wraps ErrApplyCheckDrain, so a
// caller that takes an apply refused only in its drain as applied - the
// websocket and HTTP providers answer and relay such an update as one -
// still does, and Cause, the reason the drain was rolled back.
type DrainDiscardedError struct {
	// Structs and Ranges are the parked structs and pending delete ranges
	// discarded; a range of which only a part was below the released clocks
	// counts once.
	Structs, Ranges int
	// Scanned is what the discarded drain's scans passed, counted on an
	// apply with a scan bound (WithIntegrateScanBudget) and 0 on any other.
	Scanned int
	// Senders are the origins of the applies that parked what was
	// discarded: each comparable origin once, in no order. An origin the
	// document has no record of - a park made before the option was set,
	// one whose struct a later park's merge replaced in the queue, one of
	// several that parked parts of one pending delete range, when the
	// record keeps that range under another of them (pruneParkedDeletes),
	// or one the document was told to forget (ForgetParkedOrigin) - is
	// missing, and a nil origin is left out.
	Senders []any
	// Cause is why the drain was rolled back: the apply check's refusal
	// (which wraps ErrApplyCheckDrain itself), or ErrIntegrateScanBudget,
	// ErrApplyBudget or ErrRootNotAllowed.
	Cause error
}

func (e *DrainDiscardedError) Error() string {
	return fmt.Sprintf("crdt: the drain of the pending queue was rolled back and what it released discarded: %d structs and %d delete ranges, from %d senders, its scans having passed %d: %v",
		e.Structs, e.Ranges, len(e.Senders), e.Scanned, e.Cause)
}

// Unwrap is ErrApplyCheckDrain and Cause.
func (e *DrainDiscardedError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrApplyCheckDrain}
	}
	return []error{ErrApplyCheckDrain, e.Cause}
}

// parkedDeletesBy is the delete ranges the applies of one origin parked,
// as far as the record of who parked them still holds them, and that origin.
type parkedDeletesBy struct {
	ds     DeleteSet
	origin any
}

// recordParkedOrigins keeps, on a document that discards refused drains,
// the origin of txn for every struct and delete range txn parked and the
// queue still holds at its commit, and forgets the records of delete ranges
// the queue no longer holds once they outnumber it: after every commit the
// record of delete ranges holds at most 2*pendingDsRanges+16 ranges, whatever
// frames parked them (pruneParkedDeletes). A struct leaves the record when it
// leaves the queue (forgetGoneParked), so the record of structs needs no
// prune, and it keeps slots for about four times the structs it holds at
// most (shrinkParkedBy). The caller holds d.mu.
func recordParkedOrigins(d *Doc, txn *Transaction) {
	if !d.discardRefusedDrain {
		return
	}
	s := d.store
	if rec := s.parked; rec != nil {
		if len(rec.items) > 0 && s.pending != nil {
			left := make(map[*Item]struct{}, len(rec.left))
			for _, it := range rec.left {
				left[it] = struct{}{}
			}
			for _, it := range rec.items {
				if _, gone := left[it]; it == nil || gone {
					continue
				}
				if d.parkedBy == nil {
					d.parkedBy = make(map[*Item]parkedFrom)
				}
				// A budget or roots list recorded the struct as it parked,
				// with the list the drain checks it against.
				if _, ok := d.parkedBy[it]; !ok {
					d.parkedBy[it] = parkedFrom{origin: txn.Origin}
				}
			}
			d.parkedByPeak = max(d.parkedByPeak, len(d.parkedBy))
		}
		// Only what the apply newly parked is its own. A nil origin is
		// named to no one (DrainDiscardedError.Senders), so it is not kept.
		if txn.Origin != nil {
			for _, ds := range rec.fresh {
				recordParkedDeletes(d, ds, txn.Origin)
			}
		}
	}
	if s.pending.heldStructs() == 0 {
		d.parkedBy, d.parkedByPeak = nil, 0
	}
	switch {
	case len(s.pendingDs.clients) == 0:
		d.parkedDsBy, d.parkedDsByRanges = nil, 0
	case d.parkedDsByRanges > 2*s.pendingDsRanges+16:
		pruneParkedDeletes(d)
	}
}

// shrinkParkedDeletes is by, the record of who parked each delete range,
// once sets have been cut out of it in place (ForgetParkedOrigin,
// pruneParkedDeletes): nil when it is empty, and a copy made at its length
// when it holds under a quarter of its capacity, since a slice cut in place
// keeps the array of the most sets it held.
func shrinkParkedDeletes(by []parkedDeletesBy) []parkedDeletesBy {
	switch {
	case len(by) == 0:
		return nil
	case len(by) < cap(by)/4:
		fresh := make([]parkedDeletesBy, len(by))
		copy(fresh, by)
		return fresh
	}
	return by
}

// recordParkedDeletes adds ds, the delete ranges an apply of origin newly
// parked, to the record: into the one set of origin's ranges when origin is
// comparable, compacted as the pending deletes are, so frames of one sender
// that grow or bridge the ranges it parked add none, and as a set of its own
// otherwise. It keeps parkedDsByRanges.
func recordParkedDeletes(d *Doc, ds DeleteSet, origin any) {
	var by *parkedDeletesBy
	if reflect.ValueOf(origin).Comparable() {
		for i := range d.parkedDsBy {
			if d.parkedDsBy[i].origin == origin {
				by = &d.parkedDsBy[i]
				break
			}
		}
	}
	if by == nil {
		d.parkedDsBy = append(d.parkedDsBy, parkedDeletesBy{ds: newDeleteSet(), origin: origin})
		by = &d.parkedDsBy[len(d.parkedDsBy)-1]
	}
	for client := range ds.clients {
		d.parkedDsByRanges -= len(by.ds.clients[client])
	}
	by.ds.Merge(ds)
	for client := range ds.clients {
		// The merge appends the frame's ranges to the client's and compacts
		// them in place, so the array keeps the most ranges it held
		// (DeleteSet.sortAndCompact): a sender whose frames park ranges that
		// a later frame of its own bridges into one would otherwise hold
		// that array for as long as the one range stays parked.
		by.ds.clients[client] = shrunk(by.ds.clients[client])
		d.parkedDsByRanges += len(by.ds.clients[client])
	}
}

// pruneParkedDeletes cuts the record of who parked each delete range to the
// clocks the pending deletes still hold. The sets of several origins can
// still cut one pending range into more pieces than the bound allows - one
// sender's frames parking the clocks between another's - so when more than
// 2*pendingDsRanges+16 ranges are left, each pending range the record touches
// is kept whole under one origin, that of the first set that touches it
// (onePerPendingRange). The record then holds at most pendingDsRanges ranges
// and still names an origin for every pending range it named one for. Each
// range the record holds is found in the pending deletes by a binary search,
// and the prune runs only once the record holds more than twice the pending
// ranges, so its work is what the record holds.
func pruneParkedDeletes(d *Doc) {
	s := d.store
	kept, n := d.parkedDsBy[:0], 0
	for _, by := range d.parkedDsBy {
		by.ds = heldPart(by.ds, s.pendingDs)
		if r := rangeCount(by.ds); r > 0 {
			kept = append(kept, by)
			n += r
		}
	}
	clear(d.parkedDsBy[len(kept):])
	d.parkedDsBy, d.parkedDsByRanges = kept, n
	if n > 2*s.pendingDsRanges+16 {
		d.parkedDsBy, d.parkedDsByRanges = onePerPendingRange(kept, s.pendingDs)
	}
	d.parkedDsBy = shrinkParkedDeletes(d.parkedDsBy)
}

// heldPart is the part of ds that held also holds: each range of ds cut to
// the clocks the ranges of held of the same client cover, which are sorted
// and apart, as the pending deletes keep them.
func heldPart(ds, held DeleteSet) DeleteSet {
	out := newDeleteSet()
	for client, ranges := range ds.clients {
		h := held.clients[client]
		var kept []DeleteRange
		for _, r := range ranges {
			end := r.Clock + r.Len
			i := sort.Search(len(h), func(i int) bool { return h[i].Clock+h[i].Len > r.Clock })
			for ; i < len(h) && h[i].Clock < end; i++ {
				start := max(r.Clock, h[i].Clock)
				kept = append(kept, DeleteRange{Clock: start, Len: min(end, h[i].Clock+h[i].Len) - start})
			}
		}
		if len(kept) > 0 {
			out.clients[client] = kept
		}
	}
	return out
}

// onePerPendingRange is the record by, each of whose ranges lies within one
// range of pending (heldPart), with each pending range it touches kept whole
// under the first set of by that touches it, and the ranges it then holds.
func onePerPendingRange(by []parkedDeletesBy, pending DeleteSet) ([]parkedDeletesBy, int) {
	taken := make(map[ClientID][]bool)
	out, n := by[:0], 0
	for _, b := range by {
		ds := newDeleteSet()
		for client, ranges := range b.ds.clients {
			h := pending.clients[client]
			took, ok := taken[client]
			if !ok {
				took = make([]bool, len(h))
				taken[client] = took
			}
			var whole []DeleteRange
			for _, r := range ranges {
				i := sort.Search(len(h), func(i int) bool { return h[i].Clock+h[i].Len > r.Clock })
				if !took[i] {
					took[i] = true
					whole = append(whole, h[i])
				}
			}
			if len(whole) > 0 {
				ds.clients[client] = whole
				n += len(whole)
			}
		}
		if len(ds.clients) > 0 {
			out = append(out, parkedDeletesBy{ds: ds, origin: b.origin})
		}
	}
	clear(by[len(out):])
	return out, n
}

// rangeCount is how many ranges ds holds over all its clients.
func rangeCount(ds DeleteSet) int {
	n := 0
	for _, ranges := range ds.clients {
		n += len(ranges)
	}
	return n
}

// unparkedPart is the part of ds that parked does not hold: each range of ds
// less the clocks parked's ranges of the same client cover.
func unparkedPart(ds, parked DeleteSet) DeleteSet {
	out := newDeleteSet()
	for client, ranges := range ds.clients {
		held := slices.Clone(parked.clients[client])
		slices.SortFunc(held, func(a, b DeleteRange) int { return cmp.Compare(a.Clock, b.Clock) })
		var kept []DeleteRange
		for _, r := range ranges {
			start, end := r.Clock, r.Clock+r.Len
			for _, h := range held {
				if h.Clock+h.Len <= start {
					continue
				}
				if h.Clock >= end {
					break
				}
				if h.Clock > start {
					kept = append(kept, DeleteRange{Clock: start, Len: h.Clock - start})
				}
				start = h.Clock + h.Len
				if start >= end {
					break
				}
			}
			if start < end {
				kept = append(kept, DeleteRange{Clock: start, Len: end - start})
			}
		}
		if len(kept) > 0 {
			out.clients[client] = kept
		}
	}
	return out
}

// releasedByFill is every struct of items, the pending queue as a drain
// found it, that the store s now holds the dependencies of or that another
// such struct supplies: what a drain run to its end would integrate, read
// from the clocks alone, with no integration and no scan. It returns too
// each client's clock as those structs would leave it, for the clients it
// read.
func releasedByFill(s *StructStore, items []*Item) (map[*Item]struct{}, map[ClientID]uint64) {
	reach := make(map[ClientID]uint64)
	clock := func(c ClientID) uint64 {
		if v, ok := reach[c]; ok {
			return v
		}
		v := s.NextClock(c)
		reach[c] = v
		return v
	}
	sorted := slices.Clone(items)
	slices.SortStableFunc(sorted, func(a, b *Item) int { return cmp.Compare(a.ID.Clock, b.ID.Clock) })
	released := make(map[*Item]struct{})
	for changed := true; changed; {
		changed = false
		for _, it := range sorted {
			if _, done := released[it]; done || it == nil {
				continue
			}
			c := it.ID.Client
			if it.ID.Clock > clock(c) ||
				it.Origin != nil && it.Origin.Clock >= clock(it.Origin.Client) ||
				it.OriginRight != nil && it.OriginRight.Clock >= clock(it.OriginRight.Client) ||
				it.Parent == nil && it.parentID != nil && it.parentID.Clock >= clock(it.parentID.Client) {
				continue
			}
			released[it] = struct{}{}
			reach[c] = max(clock(c), it.ID.Clock+it.Content.Len())
			changed = true
		}
	}
	return released, reach
}

// discardReleased is the rollback of a refused drain on a document that
// discards refused drains, run once takeBackDrain has left the store as the
// drain found it: the queue is parked again without what the fill released,
// and the pending deletes keep only the ranges, and the parts of ranges,
// above the clocks the store and the released structs reach. It records the
// discard in j for the apply's error (endRemoteApply): scanned is what the
// drain's scans passed, and cause why it was rolled back.
func discardReleased(txn *Transaction, j *applyJournal, scanned int, cause error) {
	d := txn.doc
	s := d.store
	var items []*Item
	if s.pending != nil {
		items = s.pending.heldItems()
	}
	released, reach := releasedByFill(s, items)
	if len(released) > 0 {
		// A new queue, so the rolled-back one, which the apply's journal holds,
		// keeps its slots.
		var kept []*Item
		for _, item := range items {
			if _, out := released[item]; !out {
				kept = append(kept, item)
			}
		}
		s.pending = nil
		if len(kept) > 0 {
			s.pending = &pendingUpdate{items: kept, bytes: itemsBytes(kept)}
		}
	}

	clock := func(c ClientID) uint64 {
		if v, ok := reach[c]; ok {
			return v
		}
		return s.NextClock(c)
	}
	found := s.pendingDs
	kept := DeleteSet{clients: make(map[ClientID][]DeleteRange, len(found.clients))}
	dropped := DeleteSet{clients: make(map[ClientID][]DeleteRange)}
	keptRanges, droppedRanges := 0, 0
	for _, client := range deleteSetClients(found) {
		below := clock(client)
		var keep, drop []DeleteRange
		for _, r := range found.clients[client] {
			switch {
			case r.Clock+r.Len <= below:
				drop = append(drop, r)
			case r.Clock < below:
				drop = append(drop, DeleteRange{Clock: r.Clock, Len: below - r.Clock})
				keep = append(keep, DeleteRange{Clock: below, Len: r.Clock + r.Len - below})
			default:
				keep = append(keep, r)
			}
		}
		if len(keep) > 0 {
			kept.clients[client] = keep
			if found.order != nil {
				kept.order = append(kept.order, client)
			}
			keptRanges += len(keep)
		}
		if len(drop) > 0 {
			dropped.clients[client] = drop
			droppedRanges += len(drop)
		}
	}
	if droppedRanges > 0 {
		// kept's map was made with room for every client the drain found,
		// and holds those with a range left: that room is its peak.
		s.pendingDs, s.pendingDsRanges = kept, keptRanges
		s.pendingDsClientsPeak = len(found.clients)
		s.shrinkPendingDsClients()
	}

	e := j.discarded
	if e == nil {
		e = &DrainDiscardedError{Cause: cause}
		j.discarded = e
	}
	e.Structs += len(released)
	e.Ranges += droppedRanges
	e.Scanned += scanned
	// This transaction's own parks have no record yet: it is made at the
	// commit (recordParkedOrigins).
	ownParks := make(map[*Item]struct{})
	var ownDeletes []DeleteSet
	if rec := s.parked; rec != nil {
		for _, it := range rec.items {
			ownParks[it] = struct{}{}
		}
		ownDeletes = rec.deletes
	}
	seen := make(map[any]struct{}, len(e.Senders))
	for _, origin := range e.Senders {
		if reflect.TypeOf(origin).Comparable() {
			seen[origin] = struct{}{}
		}
	}
	add := func(origin any) {
		if origin == nil || origin == forgottenOrigin {
			return
		}
		if reflect.TypeOf(origin).Comparable() {
			if _, dup := seen[origin]; dup {
				return
			}
			seen[origin] = struct{}{}
		}
		e.Senders = append(e.Senders, origin)
	}
	for it := range released {
		if from, ok := d.parkedBy[it]; ok {
			add(from.origin)
		} else if _, own := ownParks[it]; own {
			add(txn.Origin)
		}
		txn.noteGone(it)
	}
	if droppedRanges > 0 {
		for _, by := range d.parkedDsBy {
			if deletesOverlap(by.ds, dropped) {
				add(by.origin)
			}
		}
		for _, ds := range ownDeletes {
			if deletesOverlap(ds, dropped) {
				add(txn.Origin)
			}
		}
	}
}

// deleteSetClients is ds's clients: those its order lists, in that order,
// then any other, ascending.
func deleteSetClients(ds DeleteSet) []ClientID {
	clients := make([]ClientID, 0, len(ds.clients))
	listed := make(map[ClientID]struct{}, len(ds.order))
	for _, c := range ds.order {
		if _, ok := ds.clients[c]; ok {
			if _, dup := listed[c]; !dup {
				listed[c] = struct{}{}
				clients = append(clients, c)
			}
		}
	}
	rest := len(clients)
	for c := range ds.clients {
		if _, ok := listed[c]; !ok {
			clients = append(clients, c)
		}
	}
	slices.Sort(clients[rest:])
	return clients
}

// deletesOverlap is whether a and b share a deleted clock.
func deletesOverlap(a, b DeleteSet) bool {
	for client, ra := range a.clients {
		for _, x := range ra {
			for _, y := range b.clients[client] {
				if x.Clock < y.Clock+y.Len && y.Clock < x.Clock+x.Len {
					return true
				}
			}
		}
	}
	return false
}
