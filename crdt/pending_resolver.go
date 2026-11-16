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

// countStructRefs counts pending's runs, each one client's structs in rising
// clocks, and gives in step whether the runs' clients rise (1), as ygo writes
// an update's, fall (-1), as Yjs writes them, or neither (0).
func countStructRefs(pending []*Item) (runs, step int) {
	monotone := true
	var prev ID
	for i, item := range pending {
		id := item.ID
		if i > 0 && id.Client == prev.Client && id.Clock > prev.Clock {
			prev = id
			continue
		}
		runs++
		if i > 0 && monotone {
			if c := cmp.Compare(id.Client, prev.Client); c == 0 || (step != 0 && c != step) {
				monotone = false
			} else {
				step = c
			}
		}
		prev = id
	}
	switch {
	case !monotone:
		return runs, 0
	case step == 0:
		return runs, 1 // one run, or none
	}
	return runs, step
}

// groupStructRefs groups pending by client, each in clock order, ascending by
// client, appending the groups to into; runs and step are countStructRefs's.
// It returns the queue positions the groups index, or nil when pending already
// holds one strictly clock-ordered run per client, as a decoded update does.
func groupStructRefs(pending []*Item, runs, step int, into []structRefs) ([]structRefs, []int) {
	groups := slices.Grow(into[:0], runs)
	for i, item := range pending {
		// An ordered run's clocks may change when an overlapping item trims,
		// but its client boundary stays valid for a fallback.
		if i == 0 || item.ID.Client != pending[i-1].ID.Client || (step == 0 && item.ID.Clock <= pending[i-1].ID.Clock) {
			groups = append(groups, structRefs{client: item.ID.Client, next: i})
		}
		groups[len(groups)-1].end = i + 1
	}
	switch step {
	case 1:
		return groups, nil
	case -1:
		slices.Reverse(groups)
		return groups, nil
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
		return cmp.Compare(pending[a].ID.Clock, pending[b].ID.Clock)
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

// integratePending integrates pending in Yjs's integrateStructs order (highest
// client first, each in clock order, a blocked struct first taking the client
// it waits on), marks in done what leaves the queue, and, within an update (nil
// dropped), returns at the first refusal, as Yjs stops when a struct throws,
// where a drain drops it and goes on.
func integratePending(txn *Transaction, pending []*Item, done []bool, dropped *error) error {
	runs, step := countStructRefs(pending)
	at := len(pending)
	if step != 0 {
		var err error
		if at, err = integrateClientRuns(txn, pending, step, done, dropped); at < 0 {
			return err
		}
	}
	var buf [4]structRefs // a keystroke's groups need no allocation
	groups, order := groupStructRefs(pending, runs, step, buf[:0])
	if at < len(pending) {
		// The runs above at's client are taken, and at's run goes on from at.
		client := pending[at].ID.Client
		for i := len(groups) - 1; i >= 0 && groups[i].client >= client; i-- {
			groups[i].next = groups[i].end
			if groups[i].client == client {
				groups[i].next = at
			}
		}
	}
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

// integrateClientRuns avoids groups for a decoded update's client-ordered
// runs, step as countStructRefs gives it. It returns -1 once every run is
// taken, else the first struct that waits on a client still to take, where
// integratePending's groups go on.
func integrateClientRuns(txn *Transaction, pending []*Item, step int, done []bool, dropped *error) (int, error) {
	lo, hi := 0, len(pending) // the runs not yet taken
	var absent ClientID       // a client with no struct in [lo, hi), once known
	absentKnown := false
	for lo < hi {
		s, e := lo, hi
		if step > 0 {
			for s = hi - 1; s > lo && pending[s-1].ID.Client == pending[s].ID.Client; s-- {
			}
			hi = s
		} else {
			for e = lo + 1; e < hi && pending[e].ID.Client == pending[lo].ID.Client; e++ {
			}
			lo = e
		}
		client := pending[s].ID.Client
		for i := s; i < e; i++ {
			ok, waitsOn, err := tryIntegrateOrDrop(txn, pending[i], dropped)
			if err != nil && dropped == nil {
				done[i] = true
				return -1, err
			}
			if ok {
				done[i] = true
				continue
			}
			if waitsOn != client && (!absentKnown || waitsOn != absent) {
				if _, found := slices.BinarySearchFunc(pending[lo:hi], waitsOn, func(it *Item, c ClientID) int {
					return step * cmp.Compare(it.ID.Client, c)
				}); found {
					return i, nil
				}
				absent, absentKnown = waitsOn, true
			}
			break // the rest of the run waits behind it
		}
	}
	return -1, nil
}

// tryIntegrateOrDrop marks a panicking item done while a drain continues. The
// scan bound's and the apply budget's panics are not the item's fault: they
// go on to drainParked, and the item stays parked. A parked struct the
// allowed roots refuse is dropped, as one that panics is.
func tryIntegrateOrDrop(txn *Transaction, item *Item, dropped *error) (ok bool, waitsOn ClientID, err error) {
	if dropped == nil {
		return tryIntegrate(txn, item)
	}
	defer func() {
		if r := recover(); r != nil {
			switch r.(type) {
			case integrateScanBudgetExceeded, applyBudgetExceeded:
				panic(r)
			}
			*dropped = recoveredApplyErr(r)
			ok, waitsOn, err = true, 0, nil
		}
	}()
	return tryIntegrate(txn, item)
}

// parkWithinUpdatePending parks what the update could not integrate, merged
// into the queue as Yjs merges an update's rest into its pending update, and
// checks the persistent cap as a last guard behind the apply's check, which
// lets a queue above a lowered cap stay there but not grow.
func parkWithinUpdatePending(txn *Transaction, pending []*Item) error {
	s := txn.doc.store
	limit := max(txn.doc.maxPendingItemsLimit(), s.pending.heldStructs())
	if s.pending.heldStructs() > 0 && !s.pending.tailAppends(pending) && s.pending.mergesAny(pending) {
		pl := s.pending.plan(refsOf(pending))
		if s.pending.heldStructs()+pl.growth() > limit {
			return ErrInvalidUpdate
		}
		if j := txn.doc.applying; j != nil {
			// The merge changes the queue in place, and a rollback puts back
			// the queue the apply found.
			j.keepQueueFor(s.pending, pl)
		}
		pending = s.pending.apply(pl, pending, s.parked != nil && len(s.parked.items) > 0)
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
			s.pending.addCover(run, from)
			txn.noteParked(run)
		}
	}()
	for _, item := range pending {
		if s.pending.heldStructs() >= limit {
			return ErrInvalidUpdate
		}
		if s.pending == nil {
			s.pending = &pendingUpdate{missing: make(StateVector), tailClient: item.ID.Client, tailValid: true}
		}
		s.pending.items = append(s.pending.items, item)
		s.pending.noteTail(item)
		if client, parkedAt, future := itemFutureDep(item, s); future {
			mergePendingMissing(parkedMissing(txn), client, parkedAt)
		} else {
			mergePendingMissing(parkedMissing(txn), item.ID.Client, s.NextClock(item.ID.Client))
		}
	}
	return nil
}
