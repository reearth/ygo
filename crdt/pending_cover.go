package crdt

import (
	"cmp"
	"slices"
)

// The pending queue holds what Yjs's pending update holds, since reconnects and
// stored states re-send parked structs: a park merges an update's unintegrated
// structs into it as Yjs mergeUpdatesV2 merges rest into pending (pending_merge.go).

// clockSpan is the clocks [start, end) of one client.
type clockSpan struct{ start, end uint64 }

// pendingCover indexes a pending queue: each client's clock spans, and, once a
// park merged, its structs in clock order, with the clients in order.
type pendingCover struct {
	spans   map[ClientID]seq[clockSpan]
	structs map[ClientID]seq[queued]
	clients seq[ClientID]
}

// queued is a struct of the queue and its slot in the queue's items.
type queued struct {
	*Item
	slot int
}

// isGCStruct reports whether it is a GC placeholder (wire tag 0).
func isGCStruct(it *Item) bool { return it.Deleted && it.isGCOrphan() }

// coverOf is p's cover, built on first use and kept current by addCover.
func (p *pendingUpdate) coverOf() *pendingCover {
	if p.cover == nil {
		lists := sortedSpans(p.items)
		spans := make(map[ClientID]seq[clockSpan], len(lists))
		for client, list := range lists {
			spans[client] = seqOf(list)
		}
		p.cover = &pendingCover{spans: spans}
	}
	return p.cover
}

// sortedSpans is the merged spans of non-nil items per client, built in one
// pass for structs in clock order. A pending queue can have emptied slots.
func sortedSpans(items []*Item) map[ClientID][]clockSpan {
	spans := make(map[ClientID][]clockSpan)
	var unsorted map[ClientID]bool
	var client ClientID
	var list []clockSpan
	have := false
	for _, it := range items {
		if it == nil {
			continue
		}
		start := it.ID.Clock
		end := start + it.Content.Len()
		if end <= start {
			continue
		}
		if !have || it.ID.Client != client {
			if have {
				spans[client] = list
			}
			client, list, have = it.ID.Client, spans[it.ID.Client], true
		}
		switch n := len(list); {
		case n > 0 && start >= list[n-1].start && start <= list[n-1].end:
			list[n-1].end = max(list[n-1].end, end)
		case n > 0 && start < list[n-1].start:
			if unsorted == nil {
				unsorted = make(map[ClientID]bool)
			}
			unsorted[client] = true
			list = append(list, clockSpan{start, end})
		default:
			list = append(list, clockSpan{start, end})
		}
	}
	if have {
		spans[client] = list
	}
	for c := range unsorted {
		spans[c] = coalesce(spans[c])
	}
	return spans
}

// coalesce sorts list and merges the spans that overlap or touch.
func coalesce(list []clockSpan) []clockSpan {
	slices.SortFunc(list, func(a, b clockSpan) int { return cmp.Compare(a.start, b.start) })
	merged := list[:1]
	for _, s := range list[1:] {
		if last := &merged[len(merged)-1]; s.start <= last.end {
			last.end = max(last.end, s.end)
		} else {
			merged = append(merged, s)
		}
	}
	return merged
}

// tailAppends reports whether every item starts past every clock the queue's
// only client holds, so none overlaps a parked struct and cover is not needed.
// A GC struct may merge with the GC struct before it, so it takes no shortcut.
func (p *pendingUpdate) tailAppends(items []*Item) bool {
	if !p.tailValid {
		return false
	}
	tail := p.tail
	for _, it := range items {
		if isGCStruct(it) || it.ID.Client != p.tailClient || it.ID.Clock < tail {
			return false
		}
		tail = it.ID.Clock + it.Content.Len()
	}
	return true
}

// noteTail records item's end while one client owns every parked struct.
func (p *pendingUpdate) noteTail(item *Item) {
	if !p.tailValid {
		return
	}
	if item.ID.Client != p.tailClient {
		p.tailValid = false
		return
	}
	p.tail = max(p.tail, item.ID.Clock+item.Content.Len())
}

// addCover adds the clocks of items, just parked in p from slot from, to p's
// cover, if any.
func (p *pendingUpdate) addCover(items []*Item, from int) {
	if p.cover == nil {
		return
	}
	p.cover.addStructs(items, from)
	if len(items) <= seqFlat {
		for _, it := range items {
			p.cover.insertSpan(it)
		}
		return
	}
	for client, list := range sortedSpans(items) {
		spans := p.cover.spans[client]
		if spans.bulk(len(list)) {
			spans = seqOf(mergeSpans(spans.slice(), list))
		} else {
			for _, s := range list {
				addSpan(&spans, s)
			}
		}
		p.cover.spans[client] = spans
	}
}

// structIndex is p's cover with each client's structs in clock order, built
// on first use and kept by addCover and the merge.
func (p *pendingUpdate) structIndex() *pendingCover {
	c := p.coverOf()
	if c.structs == nil {
		lists := make(map[ClientID][]queued)
		for slot, it := range p.items {
			if it == nil {
				continue
			}
			lists[it.ID.Client] = append(lists[it.ID.Client], queued{it, slot})
		}
		c.structs = make(map[ClientID]seq[queued], len(lists))
		clients := make([]ClientID, 0, len(lists))
		for client, list := range lists {
			if !slices.IsSortedFunc(list, byClock) {
				slices.SortFunc(list, byClock)
			}
			c.structs[client] = seqOf(list)
			clients = append(clients, client)
		}
		slices.Sort(clients)
		c.clients = seqOf(clients)
	}
	return c
}

func byClock(a, b queued) int { return cmp.Compare(a.ID.Clock, b.ID.Clock) }

// addStructs adds items, parked from slot from, to the struct index, if
// built, merging what goes before a client's last struct, and new clients.
func (c *pendingCover) addStructs(items []*Item, from int) {
	if c.structs == nil {
		return
	}
	var fresh []ClientID
	var early map[ClientID][]queued
	for i, it := range items {
		q, client := queued{it, from + i}, it.ID.Client
		list := c.structs[client]
		switch n := list.len(); {
		case n == 0:
			fresh = append(fresh, client)
		case list.at(n-1).ID.Clock >= it.ID.Clock:
			if early == nil {
				early = make(map[ClientID][]queued)
			}
			early[client] = append(early[client], q)
			continue
		}
		list.insert(list.len(), q)
		c.structs[client] = list
	}
	for client, add := range early {
		slices.SortFunc(add, byClock)
		list := c.structs[client]
		list.mergeIn(add, byClock)
		c.structs[client] = list
	}
	slices.Sort(fresh)
	c.clients.mergeIn(fresh, cmp.Compare[ClientID])
}

// mergeSorted merges add into list, both sorted by compare, from the back, so
// it moves only what follows add's first element.
func mergeSorted[T any](list, add []T, compare func(a, b T) int) []T {
	if len(add) == 0 {
		return list
	}
	old := list
	list = slices.Grow(list, len(add))[:len(old)+len(add)]
	for i, j, k := len(old)-1, len(add)-1, len(list)-1; j >= 0; k-- {
		if i >= 0 && compare(old[i], add[j]) > 0 {
			list[k], i = old[i], i-1
		} else {
			list[k], j = add[j], j-1
		}
	}
	return list
}

// spliceWindows puts kepts, what the merge kept of each of a client's windows
// (nil for none it changed), in their place in the struct index, from the
// last window so the earlier keep their positions, and reports whether the
// client has none left.
func (c *pendingCover) spliceWindows(wins []queueWindow, kepts [][]queued) bool {
	client := wins[0].client
	list := c.structs[client]
	for i := len(wins) - 1; i >= 0; i-- {
		if kept := kepts[i]; kept != nil {
			if !slices.IsSortedFunc(kept, byClock) {
				slices.SortFunc(kept, byClock)
			}
			list.replace(wins[i].from, wins[i].to, kept)
		}
	}
	if list.len() > 0 {
		c.structs[client] = list
		return false
	}
	delete(c.structs, client)
	return true
}

// dropClients takes gone, clients with no struct left, out of the ordered list.
func (c *pendingCover) dropClients(gone []ClientID) {
	if len(gone) == 0 {
		return
	}
	slices.Sort(gone)
	if !c.clients.bulk(len(gone)) {
		for _, client := range gone {
			if at := c.clients.search(func(x ClientID) bool { return x >= client }); at < c.clients.len() && c.clients.at(at) == client {
				c.clients.replace(at, at+1, nil)
			}
		}
		return
	}
	clients := c.clients.slice()
	out, j := clients[:0], 0
	for _, client := range clients {
		for j < len(gone) && gone[j] < client {
			j++
		}
		if j == len(gone) || gone[j] != client {
			out = append(out, client)
		}
	}
	c.clients = seqOf(out)
}

// holdsBetween reports whether the queue holds a client between lo and hi.
func (c *pendingCover) holdsBetween(lo, hi ClientID) bool {
	at := c.clients.search(func(x ClientID) bool { return x > lo })
	return at < c.clients.len() && c.clients.at(at) < hi
}

// insertSpan adds its clocks to the cover.
func (c *pendingCover) insertSpan(it *Item) {
	start := it.ID.Clock
	end := start + it.Content.Len()
	if end <= start {
		return
	}
	spans := c.spans[it.ID.Client]
	addSpan(&spans, clockSpan{start, end})
	c.spans[it.ID.Client] = spans
}

// addSpan adds s to spans, merging the spans it overlaps or touches.
func addSpan(spans *seq[clockSpan], s clockSpan) {
	i := spans.search(func(x clockSpan) bool { return x.end >= s.start })
	j := spans.search(func(x clockSpan) bool { return x.start > s.end })
	if i < j {
		s = clockSpan{min(s.start, spans.at(i).start), max(s.end, spans.at(j-1).end)}
	}
	spans.replace(i, j, []clockSpan{s})
}

// mergeSpans merges two sorted, coalesced span lists into one.
func mergeSpans(a, b []clockSpan) []clockSpan {
	if len(a) == 0 {
		return b
	}
	if len(b) == 1 && b[0].start > a[len(a)-1].end {
		return append(a, b[0])
	}
	out := make([]clockSpan, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		var s clockSpan
		if j == len(b) || (i < len(a) && a[i].start <= b[j].start) {
			s, i = a[i], i+1
		} else {
			s, j = b[j], j+1
		}
		if n := len(out); n > 0 && s.start <= out[n-1].end {
			out[n-1].end = max(out[n-1].end, s.end)
		} else {
			out = append(out, s)
		}
	}
	return out
}

// mergesAny reports whether any of items merges with p's queue.
func (p *pendingUpdate) mergesAny(items []*Item) bool {
	for _, it := range items {
		if p.merges(it.ID.Client, it.ID.Clock, it.ID.Clock+it.Content.Len(), isGCStruct(it)) {
			return true
		}
	}
	return false
}

// merges reports whether a struct of client at [start, end) merges with p's
// queue, sharing a clock with it or, as a GC struct, touching it; p may be nil.
func (p *pendingUpdate) merges(client ClientID, start, end uint64, gc bool) bool {
	if p.heldStructs() == 0 {
		return false
	}
	if gc {
		start, end = max(start, 1)-1, end+1
	}
	spans := p.coverOf().spans[client]
	i := spans.search(func(s clockSpan) bool { return s.end > start })
	return i < spans.len() && spans.at(i).start < end
}

// heldStructs is the number of structs p's queue holds; p may be nil.
func (p *pendingUpdate) heldStructs() int {
	if p == nil {
		return 0
	}
	return len(p.items) - p.dead
}
