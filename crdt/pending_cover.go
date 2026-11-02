package crdt

import (
	"cmp"
	"slices"
	"sort"
)

// The pending queue holds what Yjs's pending update holds, since reconnects and
// stored states re-send parked structs: a park merges an update's unintegrated
// structs into it as Yjs mergeUpdatesV2 merges rest into pending (pending_merge.go).

// clockSpan is the clocks [start, end) of one client.
type clockSpan struct{ start, end uint64 }

// pendingCover indexes a pending queue: each client's clock spans, and, once a
// park merged, its structs in clock order, with the clients in order.
type pendingCover struct {
	spans   map[ClientID][]clockSpan
	structs map[ClientID][]*Item
	clients []ClientID
}

// isGCStruct reports whether it is a GC placeholder (wire tag 0).
func isGCStruct(it *Item) bool { return it.Deleted && it.isGCOrphan() }

// coverOf is p's cover, built on first use and kept current by addCover.
func (p *pendingUpdate) coverOf() *pendingCover {
	if p.cover == nil {
		p.cover = &pendingCover{spans: sortedSpans(p.items)}
	}
	return p.cover
}

// sortedSpans is the merged spans of items per client, built in one pass for
// structs in clock order.
func sortedSpans(items []*Item) map[ClientID][]clockSpan {
	spans := make(map[ClientID][]clockSpan)
	var unsorted map[ClientID]bool
	var client ClientID
	var list []clockSpan
	have := false
	for _, it := range items {
		start := it.ID.Clock
		end := start + uint64(it.Content.Len())
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

// coverInsertWork is the span-move count above which addCover merges per
// client, since insertion stops being cheaper there.
const coverInsertWork = 1 << 19

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
		tail = it.ID.Clock + uint64(it.Content.Len())
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
	p.tail = max(p.tail, item.ID.Clock+uint64(item.Content.Len()))
}

// addCover adds the clocks of items just parked in p to p's cover, if any.
func (p *pendingUpdate) addCover(items []*Item) {
	if p.cover == nil {
		return
	}
	p.cover.addStructs(items)
	work := 0
	for _, it := range items {
		if work += len(p.cover.spans[it.ID.Client]) + 1; work > coverInsertWork {
			break
		}
	}
	if work <= coverInsertWork {
		for _, it := range items {
			insertSpan(p.cover.spans, it)
		}
		return
	}
	for client, list := range sortedSpans(items) {
		p.cover.spans[client] = mergeSpans(p.cover.spans[client], list)
	}
}

// structIndex is p's cover with each client's structs in clock order, built
// on first use and kept by addCover and the merge.
func (p *pendingUpdate) structIndex() *pendingCover {
	c := p.coverOf()
	if c.structs == nil {
		c.structs = make(map[ClientID][]*Item)
		for _, it := range p.items {
			c.structs[it.ID.Client] = append(c.structs[it.ID.Client], it)
		}
		c.clients = make([]ClientID, 0, len(c.structs))
		for client, list := range c.structs {
			if !slices.IsSortedFunc(list, byClock) {
				slices.SortFunc(list, byClock)
			}
			c.clients = append(c.clients, client)
		}
		slices.Sort(c.clients)
	}
	return c
}

func byClock(a, b *Item) int { return cmp.Compare(a.ID.Clock, b.ID.Clock) }

// addStructs adds items to the struct index, if built, merging what goes
// before a client's last struct, and new clients, in one pass each.
func (c *pendingCover) addStructs(items []*Item) {
	if c.structs == nil {
		return
	}
	var fresh []ClientID
	var early map[ClientID][]*Item
	for _, it := range items {
		client := it.ID.Client
		list := c.structs[client]
		switch n := len(list); {
		case n == 0:
			fresh = append(fresh, client)
			c.structs[client] = append(list, it)
		case list[n-1].ID.Clock < it.ID.Clock:
			c.structs[client] = append(list, it)
		default:
			if early == nil {
				early = make(map[ClientID][]*Item)
			}
			early[client] = append(early[client], it)
		}
	}
	for client, add := range early {
		slices.SortFunc(add, byClock)
		c.structs[client] = mergeSorted(c.structs[client], add, byClock)
	}
	slices.Sort(fresh)
	c.clients = mergeSorted(c.clients, fresh, cmp.Compare[ClientID])
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
// (nil for none it changed), in their place in the struct index, moving the
// structs after them only from the first window that shrinks, and reports
// whether the client has none left.
func (c *pendingCover) spliceWindows(wins []queueWindow, kepts [][]*Item) bool {
	client := wins[0].client
	list := c.structs[client]
	shrink := len(wins)
	for i, w := range wins {
		kept := kepts[i]
		if kept == nil {
			continue
		}
		if !slices.IsSortedFunc(kept, byClock) {
			slices.SortFunc(kept, byClock)
		}
		if len(kept) < w.to-w.from {
			shrink = i
			break
		}
		copy(list[w.from:w.to], kept)
	}
	if shrink == len(wins) {
		return false
	}
	out, read := list[:wins[shrink].from], wins[shrink].from
	for i, w := range wins[shrink:] {
		out = append(out, list[read:w.from]...)
		kept := kepts[shrink+i]
		if kept == nil {
			kept = list[w.from:w.to]
		} else if !slices.IsSortedFunc(kept, byClock) {
			slices.SortFunc(kept, byClock)
		}
		out, read = append(out, kept...), w.to
	}
	out = append(out, list[read:]...)
	clear(list[len(out):])
	if len(out) > 0 {
		c.structs[client] = out
		return false
	}
	delete(c.structs, client)
	return true
}

// dropClients takes gone, clients with no struct left, out of the ordered list
// in one pass.
func (c *pendingCover) dropClients(gone []ClientID) {
	if len(gone) == 0 {
		return
	}
	slices.Sort(gone)
	out, j := c.clients[:0], 0
	for _, client := range c.clients {
		for j < len(gone) && gone[j] < client {
			j++
		}
		if j == len(gone) || gone[j] != client {
			out = append(out, client)
		}
	}
	c.clients = out
}

// holdsBetween reports whether the queue holds a client between lo and hi.
func (c *pendingCover) holdsBetween(lo, hi ClientID) bool {
	at, found := slices.BinarySearch(c.clients, lo)
	if found {
		at++
	}
	return at < len(c.clients) && c.clients[at] < hi
}

// insertSpan adds its clocks to m, merging the spans they overlap or touch.
func insertSpan(m map[ClientID][]clockSpan, it *Item) {
	start := it.ID.Clock
	end := start + uint64(it.Content.Len())
	if end <= start {
		return
	}
	spans := m[it.ID.Client]
	i := sort.Search(len(spans), func(k int) bool { return spans[k].end >= start })
	j := i
	for j < len(spans) && spans[j].start <= end {
		j++
	}
	if i == j {
		spans = slices.Insert(spans, i, clockSpan{start, end})
	} else {
		spans[i] = clockSpan{min(start, spans[i].start), max(end, spans[j-1].end)}
		spans = slices.Delete(spans, i+1, j)
	}
	m[it.ID.Client] = spans
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
		if p.merges(it.ID.Client, it.ID.Clock, it.ID.Clock+uint64(it.Content.Len()), isGCStruct(it)) {
			return true
		}
	}
	return false
}

// merges reports whether a struct of client at [start, end) merges with p's
// queue, sharing a clock with it or, as a GC struct, touching it; p may be nil.
func (p *pendingUpdate) merges(client ClientID, start, end uint64, gc bool) bool {
	if p == nil || len(p.items) == 0 {
		return false
	}
	if gc {
		start, end = max(start, 1)-1, end+1
	}
	spans := p.coverOf().spans[client]
	i := sort.Search(len(spans), func(k int) bool { return spans[k].end > start })
	return i < len(spans) && spans[i].start < end
}

// heldStructs is the number of structs p's queue holds; p may be nil.
func (p *pendingUpdate) heldStructs() int {
	if p == nil {
		return 0
	}
	return len(p.items)
}
