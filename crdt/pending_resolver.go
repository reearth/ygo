package crdt

import (
	"cmp"
	"slices"
)

// structRefs is one client's structs in clock order, as an entry of Yjs's
// clientsStructRefs: positions [next, end) of the grouped queue are left.
type structRefs struct {
	client    ClientID
	next, end int
}

// groupStructRefs groups pending by client, each in clock order, ascending by
// client, appending the groups to into. It returns the queue positions the
// groups index, or nil when pending already holds one strictly clock-ordered
// run per client, as a decoded update does. Of two queued copies of a clock, a
// GC struct goes first: it waits only for its own client, so a copy blocked on
// another client cannot hold it back.
func groupStructRefs(pending []*Item, into []structRefs) ([]structRefs, []int) {
	runs := 0
	for i, item := range pending {
		if i == 0 || item.ID.Client != pending[i-1].ID.Client || item.ID.Clock <= pending[i-1].ID.Clock {
			runs++
		}
	}
	groups := slices.Grow(into[:0], runs)
	for i, item := range pending {
		if i == 0 || item.ID.Client != pending[i-1].ID.Client || item.ID.Clock <= pending[i-1].ID.Clock {
			groups = append(groups, structRefs{client: item.ID.Client, next: i})
		}
		groups[len(groups)-1].end = i + 1
	}
	slices.SortFunc(groups, func(a, b structRefs) int { return cmp.Compare(a.client, b.client) })
	unique := true
	for i := 1; i < len(groups) && unique; i++ {
		unique = groups[i].client != groups[i-1].client
	}
	if unique {
		return groups, nil
	}
	order := make([]int, len(pending))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if c := cmp.Compare(pending[a].ID.Client, pending[b].ID.Client); c != 0 {
			return c
		}
		if c := cmp.Compare(pending[a].ID.Clock, pending[b].ID.Clock); c != 0 {
			return c
		}
		return cmp.Compare(copyRank(pending[a]), copyRank(pending[b]))
	})
	groups = groups[:0]
	for i, p := range order {
		if i == 0 || pending[p].ID.Client != pending[order[i-1]].ID.Client {
			groups = append(groups, structRefs{client: pending[p].ID.Client, next: i})
		}
		groups[len(groups)-1].end = i + 1
	}
	return groups, order
}

// copyRank orders a GC struct before another struct at the same clock.
func copyRank(it *Item) int {
	if isGCStruct(it) {
		return 0
	}
	return 1
}

// integratePending integrates pending in Yjs's integrateStructs order (highest
// client first, each in clock order, a blocked struct first taking the client
// it waits on), marks in done what leaves the queue, and, within an update (nil
// dropped), returns at the first refusal, as Yjs stops when a struct throws,
// where a drain drops it and goes on.
func integratePending(txn *Transaction, pending []*Item, done []bool, dropped *error) error {
	var buf [4]structRefs // a keystroke's groups need no allocation
	groups, order := groupStructRefs(pending, buf[:0])
	take := func(g *structRefs) int {
		p := g.next
		g.next++
		if order != nil {
			return order[p]
		}
		return p
	}
	current := len(groups) - 1
	find := func(client ClientID) *structRefs {
		if current >= 0 && groups[current].client == client {
			return &groups[current] // most lookups are for the group being drained
		}
		lo, hi := 0, len(groups)
		for lo < hi {
			m := int(uint(lo+hi) >> 1)
			if groups[m].client < client {
				lo = m + 1
			} else {
				hi = m
			}
		}
		if lo < len(groups) && groups[lo].client == client {
			return &groups[lo]
		}
		return nil
	}
	var stackBuf [8]int
	stack := stackBuf[:0]
	for {
		var head int
		if n := len(stack); n > 0 {
			head, stack = stack[n-1], stack[:n-1]
		} else {
			for current >= 0 && groups[current].next == groups[current].end {
				current--
			}
			if current < 0 {
				return nil
			}
			head = take(&groups[current])
		}
		for {
			item := pending[head]
			ok, waitsOn, err := tryIntegrateOrDrop(txn, item, dropped)
			if err != nil && dropped == nil {
				done[head] = true
				return err
			}
			if ok {
				done[head] = true
				break
			}
			if waitsOn != item.ID.Client {
				if g := find(waitsOn); g != nil && g.next < g.end {
					stack = append(stack, head)
					head = take(g)
					continue
				}
			}
			stack = append(stack, head)
			for _, blocked := range stack {
				g := find(pending[blocked].ID.Client)
				g.next = g.end
			}
			stack = stack[:0]
			break
		}
	}
}

// tryIntegrateOrDrop is tryIntegrate that, with a non-nil dropped, reports a
// panicking item as integrated so the caller stops retrying it.
func tryIntegrateOrDrop(txn *Transaction, item *Item, dropped *error) (ok bool, waitsOn ClientID, err error) {
	if dropped == nil {
		return tryIntegrate(txn, item)
	}
	defer func() {
		if r := recover(); r != nil {
			*dropped = integrationPanicErr(r)
			ok, waitsOn, err = true, 0, nil
		}
	}()
	return tryIntegrate(txn, item)
}

// parkWithinUpdatePending parks what the update could not integrate, minus
// clocks the queue already holds, as Yjs merges its pending structs, and
// charges only those to the persistent cap, as a last guard behind the apply's
// check.
func parkWithinUpdatePending(txn *Transaction, pending []*Item) error {
	s := txn.doc.store
	// The cover is built before the park adds to it, from the queue as it was.
	held := s.pending != nil && len(s.pending.items) > 0
	tailAppend := held && s.pending.tailAppends(pending)
	if held && !tailAppend {
		s.pending.coverOf()
	}
	from := 0
	if s.pending != nil {
		from = len(s.pending.items)
	}
	defer func() {
		// The run this park appended, also when the cap stops it partway.
		if s.pending != nil && len(s.pending.items) > from {
			n := len(s.pending.items)
			run := s.pending.items[from:n:n]
			s.pending.addCover(run)
			txn.noteParked(run)
		}
	}()
	for _, item := range pending {
		if held && !tailAppend {
			if item = unparked(s, item, s.NextClock(item.ID.Client)); item == nil {
				continue
			}
		}
		if s.pending != nil && len(s.pending.items) >= txn.doc.maxPendingItemsLimit() {
			return ErrInvalidUpdate
		}
		if s.pending == nil {
			s.pending = &pendingUpdate{missing: make(StateVector), tailClient: item.ID.Client, tailValid: true}
		}
		s.pending.items = append(s.pending.items, item)
		s.pending.noteTail(item)
		if client, parkedAt, future := itemFutureDep(item, s); future {
			mergePendingMissing(s.pending.missing, client, parkedAt)
		} else {
			mergePendingMissing(s.pending.missing, item.ID.Client, s.NextClock(item.ID.Client))
		}
	}
	return nil
}
