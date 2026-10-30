package crdt

import (
	"cmp"
	"slices"
	"sort"
)

// The pending queue holds each clock once, as Yjs does, because reconnects
// and stored states re-send structs it already parks.

// clockSpan is the clocks [start, end) of one client.
type clockSpan struct{ start, end uint64 }

// pendingCover is the clock spans a pending queue holds per client, with GC
// structs kept apart because only a parked GC struct holds a GC copy.
type pendingCover struct {
	spans map[ClientID][]clockSpan
	gc    map[ClientID][]clockSpan
}

// isGCStruct reports whether it is a GC placeholder (wire tag 0).
func isGCStruct(it *Item) bool { return it.Deleted && it.isGCOrphan() }

func (c *pendingCover) of(gc bool) map[ClientID][]clockSpan {
	if gc {
		return c.gc
	}
	return c.spans
}

// coverOf is p's cover, built on first use and kept current by addCover.
func (p *pendingUpdate) coverOf() *pendingCover {
	if p.cover == nil {
		p.cover = &pendingCover{spans: sortedSpans(p.items, false), gc: sortedSpans(p.items, true)}
	}
	return p.cover
}

// sortedSpans is the merged spans of items (GC structs only when gcOnly) per
// client, built in one pass for structs in clock order.
func sortedSpans(items []*Item, gcOnly bool) map[ClientID][]clockSpan {
	spans := make(map[ClientID][]clockSpan)
	var unsorted map[ClientID]bool
	var client ClientID
	var list []clockSpan
	have := false
	for _, it := range items {
		if gcOnly && !isGCStruct(it) {
			continue
		}
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

// addCover adds the clocks of items just parked in p to p's cover, if any.
func (p *pendingUpdate) addCover(items []*Item) {
	if p.cover == nil {
		return
	}
	work := 0
	for _, it := range items {
		work += len(p.cover.spans[it.ID.Client]) + 1
		if isGCStruct(it) {
			work += len(p.cover.gc[it.ID.Client]) + 1
		}
		if work > coverInsertWork {
			break
		}
	}
	if work <= coverInsertWork {
		for _, it := range items {
			insertSpan(p.cover.spans, it)
			if isGCStruct(it) {
				insertSpan(p.cover.gc, it)
			}
		}
		return
	}
	for _, gc := range []bool{false, true} {
		add := sortedSpans(items, gc)
		m := p.cover.of(gc)
		for client, list := range add {
			m[client] = mergeSpans(m[client], list)
		}
	}
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

// heldUntil is the first clock of [max(clock, from), end) of client that p
// does not hold (as GC structs, when gc), or end; p may be nil.
func (p *pendingUpdate) heldUntil(client ClientID, clock, from, end uint64, gc bool) uint64 {
	start := max(clock, from)
	if p == nil || start >= end {
		return start
	}
	spans := p.coverOf().of(gc)[client]
	i := sort.Search(len(spans), func(k int) bool { return spans[k].end > start })
	if i == len(spans) || spans[i].start > start {
		return start
	}
	return min(spans[i].end, end)
}

// holds reports whether p holds every clock of [max(clock, from), end) of
// client (as GC structs, when gc), so a struct spanning them parks nothing.
func (p *pendingUpdate) holds(client ClientID, clock, from, end uint64, gc bool) bool {
	return p.heldUntil(client, clock, from, end, gc) >= end
}

// heldTailFrom is where the span of p holding end-1 of [start, end) starts,
// when it starts after start, or else end.
func (p *pendingUpdate) heldTailFrom(client ClientID, start, end uint64, gc bool) uint64 {
	spans := p.coverOf().of(gc)[client]
	i := sort.Search(len(spans), func(k int) bool { return spans[k].end >= end })
	if i == len(spans) || spans[i].start >= end || spans[i].start <= start {
		return end
	}
	return spans[i].start
}

// unparked is item without the clocks the pending queue already holds at its
// start and end, as Yjs merges pending structs.
func unparked(s *StructStore, item *Item, from uint64) *Item {
	p := s.pending
	if p == nil {
		return item
	}
	client := item.ID.Client
	gc := isGCStruct(item)
	end := item.ID.Clock + uint64(item.Content.Len())
	at := p.heldUntil(client, item.ID.Clock, from, end, gc)
	if at >= end {
		return nil
	}
	if at > max(item.ID.Clock, from) {
		sliceItemFront(item, int(at-item.ID.Clock))
	}
	if tail := p.heldTailFrom(client, max(at, item.ID.Clock), end, gc); tail < end {
		item.Content.Splice(int(tail - item.ID.Clock))
	}
	return item
}
