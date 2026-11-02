package crdt

import (
	"cmp"
	"math"
	"slices"
	"sort"
)

// mergeDropped marks a struct the merge leaves out.
const mergeDropped = math.MaxUint64

// mergeRef is what Yjs's merge reads of a struct: its clocks and whether it is
// a GC struct.
type mergeRef struct {
	client     ClientID
	clock, end uint64
	gc         bool
}

func refOf(it *Item) mergeRef {
	return mergeRef{client: it.ID.Client, clock: it.ID.Clock, end: it.ID.Clock + uint64(it.Content.Len()), gc: isGCStruct(it)}
}

// mergeKeep is the clocks [start, end) the merge keeps of a struct, or none.
type mergeKeep struct {
	start, end uint64
}

func (k mergeKeep) kept() bool { return k.start != mergeDropped }

// mergeSide is one input of yjsMerge, in mergeOrder.
type mergeSide struct {
	list []mergeRef
	keep []mergeKeep
	pos  int
}

func (d *mergeSide) curr() *mergeRef {
	if d.pos < len(d.list) {
		return &d.list[d.pos]
	}
	return nil
}

// mergeWrite is Yjs's currWrite: the struct being written, or a skip.
type mergeWrite struct {
	client   ClientID
	end      uint64
	gc, skip bool
	owner    *mergeKeep // the kept struct the write is, if not a skip
}

// take makes d's current struct the write from start, and moves d on.
func (d *mergeSide) take(start uint64) mergeWrite {
	r := d.list[d.pos]
	k := &d.keep[d.pos]
	*k = mergeKeep{start: start, end: r.end}
	d.pos++
	return mergeWrite{client: r.client, end: r.end, gc: r.gc, owner: k}
}

// mergeBefore is Yjs's decoder comparator: the higher client, then the lower
// clock, and at one clock a GC struct and an item swap.
func mergeBefore(b, a *mergeRef) bool {
	if b.client != a.client {
		return b.client > a.client
	}
	if b.clock != a.clock {
		return b.clock < a.clock
	}
	return b.gc != a.gc
}

// yjsMerge keeps of held (the queue) and arrived what Yjs
// mergeUpdatesV2([pending, rest]) keeps, restart naming where Yjs's decoder
// order starts afresh because a struct the inputs leave out came between.
func yjsMerge(held, arrived []mergeRef, keepHeld, keepArrived []mergeKeep, restart func(ClientID) bool) {
	for i := range keepHeld {
		keepHeld[i] = mergeKeep{start: mergeDropped}
	}
	for i := range keepArrived {
		keepArrived[i] = mergeKeep{start: mergeDropped}
	}
	sides := [2]mergeSide{{list: held, keep: keepHeld}, {list: arrived, keep: keepArrived}}
	order := [2]*mergeSide{&sides[0], &sides[1]}
	n := 2
	var w mergeWrite
	writing, at, atSet := false, ClientID(0), false
	for {
		k := 0
		for _, d := range order[:n] {
			if d.curr() != nil {
				order[k] = d
				k++
			}
		}
		if n = k; n == 0 {
			return
		}
		if n == 2 {
			if c := max(order[0].curr().client, order[1].curr().client); !atSet || c != at {
				at, atSet = c, true
				if restart(c) {
					order = [2]*mergeSide{&sides[0], &sides[1]}
				}
			}
			if mergeBefore(order[1].curr(), order[0].curr()) {
				order[0], order[1] = order[1], order[0]
			}
		}
		d := order[0]
		first := d.curr().client
		if !writing {
			w, writing = d.take(d.curr().clock), true
		} else {
			curr := d.curr()
			iterated := false
			for curr != nil && curr.end <= w.end && curr.client >= w.client {
				d.pos++
				curr = d.curr()
				iterated = true
			}
			if curr == nil || curr.client != first || (iterated && curr.clock > w.end) {
				continue
			}
			switch {
			case first != w.client:
				w = d.take(curr.clock)
			case w.end < curr.clock:
				if w.skip {
					w.end = curr.end
				} else {
					w = mergeWrite{client: first, end: curr.clock, skip: true}
				}
			default:
				start := curr.clock
				if diff := w.end - start; diff > 0 {
					if w.skip {
						w.end -= diff
					} else {
						start += diff
					}
				}
				if !w.skip && w.gc && curr.gc {
					// The GC struct merges into the write, and d stays where it is.
					w.end = curr.end
					w.owner.end = w.end
				} else {
					w = d.take(start)
				}
			}
		}
		for next := d.curr(); next != nil && next.client == first && next.clock == w.end; next = d.curr() {
			w = d.take(next.clock)
		}
	}
}

// mergeOrder sorts refs as an update holds them and returns where each was.
func mergeOrder(refs []mergeRef) []int {
	order := make([]int, len(refs))
	for i := range order {
		order[i] = i
	}
	less := func(a, b mergeRef) int {
		if c := cmp.Compare(b.client, a.client); c != 0 {
			return c
		}
		return cmp.Compare(a.clock, b.clock)
	}
	if slices.IsSortedFunc(refs, less) {
		return order
	}
	slices.SortStableFunc(order, func(a, b int) int { return less(refs[a], refs[b]) })
	sorted := make([]mergeRef, len(refs))
	for i, p := range order {
		sorted[i] = refs[p]
	}
	copy(refs, sorted)
	return order
}

// mergePlan is what merging arrived into p's queue keeps of the windows'
// structs, held, and of each arrival.
type mergePlan struct {
	wins                  []queueWindow
	held                  []*Item
	keepHeld, keepArrived []mergeKeep
}

// queueWindow is the queue structs list[from:to] of client that take part in
// a merge, with tail standing in for the client's structs past them, and the
// last chain of them touching GC structs past the arrivals, from chainStart.
type queueWindow struct {
	client     ClientID
	list       []*Item
	from, to   int
	tail       []mergeRef
	chain      int
	chainStart *Item
}

// plan merges arrived into p's queue, reading of each arrival client only the
// queue structs that touch its arrivals and the one before each run of them.
func (p *pendingUpdate) plan(arrived []mergeRef) mergePlan {
	in := slices.Clone(arrived)
	ao := mergeOrder(in)
	c := p.structIndex()
	// A GC write takes in a whole chain of touching GC structs after it or
	// none, so a chain is read past its first struct only once that is taken.
	whole := make(map[*Item]bool)
	for {
		if pl, done := c.planChains(in, ao, whole); done {
			return pl
		}
	}
}

// gcChain reports whether list[to] is a GC struct touching list[to-1].
func gcChain(list []*Item, to int) bool {
	return to > 0 && to < len(list) && isGCStruct(list[to]) &&
		list[to].ID.Clock <= list[to-1].ID.Clock+uint64(list[to-1].Content.Len())
}

// planChains plans the merge reading of each touching GC chain past a window
// its first struct, or all of it when whole names it, and reports whether
// that was enough; if not, it marks the chain in whole.
func (c *pendingCover) planChains(in []mergeRef, ao []int, whole map[*Item]bool) (mergePlan, bool) {
	var pl mergePlan
	restarts := make(map[ClientID]bool)
	for i := 0; i < len(in); {
		client := in[i].client
		j := i + 1
		for j < len(in) && in[j].client == client {
			j++
		}
		list := c.structs[client]
		for k := i; k < j; {
			lo, hi := in[k].clock, in[k].end
			from := sort.Search(len(list), func(n int) bool { return list[n].ID.Clock+uint64(list[n].Content.Len()) >= lo })
			to, chain := -1, 0
			var start *Item
			// Arrivals whose structs a window shares join it; a window only
			// grows, so a chain read whole stays read.
			for m := k + 1; ; m++ {
				if t := sort.Search(len(list), func(n int) bool { return list[n].ID.Clock > hi }); t > to {
					to, start, chain = t, nil, 0
					if gcChain(list, to) {
						start = list[to]
						for chain == 0 || (whole[start] && gcChain(list, to)) {
							to, chain = to+1, chain+1
						}
					}
				}
				if m == j {
					k = m
					break
				}
				next := sort.Search(len(list), func(n int) bool { return list[n].ID.Clock+uint64(list[n].Content.Len()) >= in[m].clock })
				if max(next, 1)-1 >= to {
					k = m
					break
				}
				hi = max(hi, in[m].end)
			}
			if from > 0 {
				from-- // the struct before the window sets the decoder order for it
			} else if len(pl.wins) > 0 && pl.wins[len(pl.wins)-1].client != client && to > 0 && list[0].ID.Clock == lo {
				// A first step at one clock goes by the decoder order the client
				// before leaves, which a client only the queue holds resets.
				prev := &pl.wins[len(pl.wins)-1]
				if restarts[client] = c.holdsBetween(client, prev.client); !restarts[client] {
					prev.tail = c.tailRefs(prev.list[prev.to:])
				}
			}
			pl.wins = append(pl.wins, queueWindow{client: client, list: list, from: from, to: to, chain: chain, chainStart: start})
		}
		i = j
	}
	var held []mergeRef
	var real []int // where pl.held's structs are in held
	for wi, w := range pl.wins {
		if wi > 0 && pl.wins[wi-1].client == w.client && pl.wins[wi-1].to < w.from {
			held = append(held, c.gapRefs(w.list[pl.wins[wi-1].to:w.from])...)
		}
		for _, it := range w.list[w.from:w.to] {
			pl.held = append(pl.held, it)
			real = append(real, len(held))
			held = append(held, refOf(it))
		}
		held = append(held, w.tail...)
	}
	hk, ak := make([]mergeKeep, len(held)), make([]mergeKeep, len(in))
	yjsMerge(held, in, hk, ak, func(c ClientID) bool { return restarts[c] })
	pl.keepHeld, pl.keepArrived = make([]mergeKeep, len(pl.held)), make([]mergeKeep, len(in))
	for i, q := range real {
		pl.keepHeld[i] = hk[q]
	}
	for i, q := range ao {
		pl.keepArrived[q] = ak[i]
	}
	done, at := true, 0
	for _, w := range pl.wins {
		at += w.to - w.from
		if w.chain > 0 && !whole[w.chainStart] && gcChain(w.list, w.to) && !pl.keepHeld[at-1].kept() {
			whole[w.chainStart], done = true, false
		}
	}
	return pl, done
}

// tailRefs stands in for past, which the merge keeps whole and reads only for
// its decoder order, with its first run of touching structs and one more.
func (c *pendingCover) tailRefs(past []*Item) []mergeRef {
	if len(past) == 0 {
		return nil
	}
	first := refOf(past[0])
	spans := c.spans[first.client]
	s := sort.Search(len(spans), func(k int) bool { return spans[k].end > first.clock })
	refs := []mergeRef{{client: first.client, clock: first.clock, end: spans[s].end, gc: first.gc}}
	if s+1 < len(spans) {
		refs = append(refs, mergeRef{client: first.client, clock: spans[s+1].start, end: spans[s+1].end})
	}
	return refs
}

// gapRefs stands in for gap, a client's structs between two of its windows,
// which the merge keeps whole and reads only for its decoder order: the run of
// touching structs from the first of them, and one struct to the last one's end.
func (c *pendingCover) gapRefs(gap []*Item) []mergeRef {
	first, last := refOf(gap[0]), refOf(gap[len(gap)-1])
	spans := c.spans[first.client]
	s := sort.Search(len(spans), func(k int) bool { return spans[k].end > first.clock })
	end := min(spans[s].end, last.end)
	refs := []mergeRef{{client: first.client, clock: first.clock, end: end, gc: first.gc}}
	if end < last.end {
		refs = append(refs, mergeRef{client: first.client, clock: spans[s+1].start, end: last.end})
	}
	return refs
}

// growth is how many structs the plan adds to the queue.
func (pl mergePlan) growth() int {
	n := -len(pl.keepHeld)
	for _, k := range pl.keepHeld {
		if k.kept() {
			n++
		}
	}
	for _, k := range pl.keepArrived {
		if k.kept() {
			n++
		}
	}
	return n
}

// keep cuts item to what k keeps of it, or reports it dropped.
func keep(item *Item, k mergeKeep) bool {
	if !k.kept() {
		return false
	}
	if k.start > item.ID.Clock {
		sliceItemFront(item, int(k.start-item.ID.Clock))
		if !isGCStruct(item) {
			// Yjs writes no parent for a cut struct, which then takes its origin's.
			item.Parent, item.ParentSub, item.parentID = nil, nil, nil
		}
	}
	if n := int(k.end - k.start); n > item.Content.Len() {
		// GC structs after it merged into this GC struct.
		item.Content = NewContentDeleted(n)
	}
	return true
}

func refsOf(items []*Item) []mergeRef {
	refs := make([]mergeRef, len(items))
	for i, it := range items {
		refs[i] = refOf(it)
	}
	return refs
}

// apply merges arrived into p's queue by plan and returns the arrivals to
// append, cutting the queue in place unless a recorded view must stay.
func (p *pendingUpdate) apply(pl mergePlan, arrived []*Item, viewed bool) []*Item {
	var dropped []*Item
	var gone []ClientID
	held := pl.held
	kepts := make([][]*Item, len(pl.wins)) // nil where the merge changed nothing
	for wi, w := range pl.wins {
		n := w.to - w.from
		win, keeps := held[:n], pl.keepHeld[:n]
		held, pl.keepHeld = held[n:], pl.keepHeld[n:]
		kept, changed := make([]*Item, 0, n), false
		for i, it := range win {
			k, end := keeps[i], it.ID.Clock+uint64(it.Content.Len())
			switch {
			case k == mergeKeep{start: it.ID.Clock, end: end}:
				kept = append(kept, it)
			case !k.kept():
				dropped, changed = append(dropped, it), true
			default:
				// A GC struct grows by the GC structs it took in.
				keep(it, k)
				if k.end > end {
					insertSpan(p.cover.spans, it)
					p.noteTail(it)
				}
				kept, changed = append(kept, it), true
			}
		}
		if changed {
			kepts[wi] = kept
		}
	}
	for a := 0; a < len(pl.wins); {
		b := a + 1
		for b < len(pl.wins) && pl.wins[b].client == pl.wins[a].client {
			b++
		}
		if slices.ContainsFunc(kepts[a:b], func(k []*Item) bool { return k != nil }) && p.cover.spliceWindows(pl.wins[a:b], kepts[a:b]) {
			gone = append(gone, pl.wins[a].client)
		}
		a = b
	}
	p.cover.dropClients(gone)
	if len(dropped) > 0 {
		p.items = withoutItems(p.items, dropped, viewed)
	}
	out := arrived[:0]
	for i, it := range arrived {
		if keep(it, pl.keepArrived[i]) {
			out = append(out, it)
		}
	}
	return out
}

// withoutItems is items less dropped, in place unless viewed.
func withoutItems(items, dropped []*Item, viewed bool) []*Item {
	var set map[*Item]struct{}
	if len(dropped) > 8 {
		set = make(map[*Item]struct{}, len(dropped))
		for _, it := range dropped {
			set[it] = struct{}{}
		}
	}
	first := 0
	for first < len(items) && !isDropped(items[first], dropped, set) {
		first++
	}
	out := items[:first]
	if viewed {
		out = append(make([]*Item, 0, len(items)-len(dropped)), out...)
	}
	for _, it := range items[min(first+1, len(items)):] {
		if !isDropped(it, dropped, set) {
			out = append(out, it)
		}
	}
	if !viewed {
		clear(items[len(out):])
	}
	return out
}

func isDropped(it *Item, dropped []*Item, set map[*Item]struct{}) bool {
	if set != nil {
		_, ok := set[it]
		return ok
	}
	for _, d := range dropped {
		if it == d {
			return true
		}
	}
	return false
}

// checkCollected refuses the update when what it parks, by the walk over its
// collected blocked groups, would pass the room; over a queue it merges them in.
func (b *pendingBudget) checkCollected(known StateVector, bounds []pendingWireBound) error {
	// A sizing pass makes the retained records and summary exact.
	merging := b.parked.heldStructs() > 0 // groups that can never integrate are kept for the merge
	sizing := pendingCollector{permanent: merging}
	if _, err := scanPendingGroupsBounded(b.scanner(), known, b.remaining, &sizing, bounds, nil, b.superseded); err != nil {
		return wrapUpdateErr(err)
	}
	collect := pendingCollector{fill: true, permanent: merging, groups: make([]pendingGroup, 0, sizing.heads), sum: make([]byte, 0, sizing.size)}
	result, err := scanPendingGroupsBounded(b.scanner(), known, b.remaining, &collect, bounds, nil, b.superseded)
	if err != nil {
		return wrapUpdateErr(err)
	}
	groups := collect.groups
	resolvePendingGroups(groups, collect.sum, known)
	pending := 0
	if !merging {
		pending = result.permanent
	}
	var arrived []mergeRef
	merges := false
	for i := range groups {
		// From where a group waits, all of it parks, as in Yjs's rest.
		for g := &groups[i]; !g.done; g.next(collect.sum) {
			pending++
			if !merging {
				if pending > b.remaining {
					return ErrInvalidUpdate
				}
				continue
			}
			arrived = append(arrived, mergeRef{client: g.client, clock: g.clock, end: g.end, gc: g.gc})
			merges = merges || b.parked.merges(g.client, g.clock, g.end, g.gc)
		}
	}
	if merges {
		pending = b.parked.plan(arrived).growth()
	}
	if pending > b.remaining {
		return ErrInvalidUpdate
	}
	return nil
}
