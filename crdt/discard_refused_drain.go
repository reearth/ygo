package crdt

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
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
// deletions it recorded.
func (d *Doc) SetDiscardRefusedDrain(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.discardRefusedDrain = on
	if !on {
		d.parkedDsBy = nil
	}
}

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
	// or one whose struct a later park's merge replaced in the queue - is
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

// parkedDeletesBy is a delete set an apply parked and that apply's origin.
type parkedDeletesBy struct {
	ds     DeleteSet
	origin any
}

// recordParkedOrigins keeps, on a document that discards refused drains,
// the origin of txn for every struct and delete set txn parked and the queue
// still holds at its commit, and forgets the records of what the queue no
// longer holds once they outnumber it. The caller holds d.mu.
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
		}
		for _, ds := range rec.deletes {
			d.parkedDsBy = append(d.parkedDsBy, parkedDeletesBy{ds: copyDeleteSet(ds), origin: txn.Origin})
		}
	}
	held := s.pending.heldStructs()
	switch {
	case held == 0:
		d.parkedBy = nil
	case len(d.parkedBy) > 2*held+16:
		kept := make(map[*Item]parkedFrom, held)
		for _, it := range s.pending.heldItems() {
			if from, ok := d.parkedBy[it]; ok {
				kept[it] = from
			}
		}
		d.parkedBy = kept
	}
	switch {
	case len(s.pendingDs.clients) == 0:
		d.parkedDsBy = nil
	case len(d.parkedDsBy) > 2*s.pendingDsRanges+16:
		d.parkedDsBy = slices.DeleteFunc(d.parkedDsBy, func(by parkedDeletesBy) bool {
			return !deletesOverlap(by.ds, s.pendingDs)
		})
	}
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
			s.pending = &pendingUpdate{items: kept}
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
		s.pendingDs, s.pendingDsRanges = kept, keptRanges
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
		if origin == nil {
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
