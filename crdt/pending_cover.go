package crdt

import (
	"cmp"
	"slices"
	"sort"
)

// A struct the pending queue already holds is not parked again. A peer that
// reconnects sends again what the room has not acknowledged, its parked tail
// included, and a stored state carries the parked structs it was saved with,
// so the same structs arrive more than once while their predecessor is still
// missing. Yjs merges its pending structs with each such arrival and holds
// each clock once.
//
// Struct IDs are unique to their content, so a struct whose clocks the queue
// already spans is the struct the queue holds. What an update leaves to park
// is merged with the queue (parkWithinUpdatePending), after the update has
// integrated what it can: a struct the queue holds parks no second time, one
// whose first or last clocks it holds parks only the clocks it lacks, as Yjs
// slices it, and the pending budget does not count a struct it holds.

// clockSpan is the clocks [start, end) of one client.
type clockSpan struct{ start, end uint64 }

// pendingCover is the clocks a pending queue's items span, per client, as
// sorted, disjoint, non-adjacent spans: spans for every parked struct, gc for
// the parked GC structs alone. A GC struct waits only for its client's clocks
// before it, so a GC copy of clocks the queue holds as an item can integrate
// where the item cannot: a GC struct is held only by a parked GC struct.
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

// coverOf is p's cover, built when first asked for and kept by addCover.
// Every other writer of a queue builds a new pendingUpdate, with no cover.
func (p *pendingUpdate) coverOf() *pendingCover {
	if p.cover == nil {
		p.cover = &pendingCover{spans: sortedSpans(p.items, false), gc: sortedSpans(p.items, true)}
	}
	return p.cover
}

// sortedSpans is the merged spans of items (of its GC structs alone when
// gcOnly), per client. A struct that starts at or after its client's last
// span joins or follows it in place, so items in clock order, as a load or a
// drain parks them, cost one pass and one span per run; a client whose
// structs arrived out of order is sorted and merged once at the end.
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

// coverInsertWork bounds the spans addCover moves inserting structs one by
// one: each insertion copies the spans after it, so k structs among s spans
// of their client move up to k·s, with almost no allocation, where one merge
// per client costs O(s + k log k) and allocates the merged lists. They cost
// the same near k·s = 460,000 at 10,000 spans (k = 46) and 650,000 at 100,000
// (k = 6.5) (BenchmarkPendingParkBatch).
const coverInsertWork = 1 << 19

// addCover adds the clocks of items, structs just parked in p, to p's cover
// when p has one: by insertion while the spans that moves stay under
// coverInsertWork, otherwise by one merge per client.
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

// insertSpan adds its clocks to m wherever they fall, merging the spans they
// overlap or touch.
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

// heldUntil is the first clock of [max(clock, from), end) of client that no
// struct parked in p holds (no parked GC struct, when gc), or end when parked
// structs hold them all. from is the clock below which the clocks are the
// store's. p may be nil.
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

// heldTailFrom is the first clock of the span of p (of p's GC structs, when
// gc) that holds the end of [start, end) of client, when that span starts
// after start, or end when no span holds end-1 or one holds start too.
func (p *pendingUpdate) heldTailFrom(client ClientID, start, end uint64, gc bool) uint64 {
	spans := p.coverOf().of(gc)[client]
	i := sort.Search(len(spans), func(k int) bool { return spans[k].end >= end })
	if i == len(spans) || spans[i].start >= end || spans[i].start <= start {
		return end
	}
	return spans[i].start
}

// unparked is item, a struct an update leaves to park, without the clocks the
// store's pending queue already holds at its start and at its end, as Yjs
// merges a pending struct with the pending structs it holds. A GC struct
// waits only for its client's earlier clocks, so it is held only by parked
// GC structs: a GC copy of clocks the queue holds as an item parks, and
// integrates once the clocks before it arrive, as in Yjs. from is the store's
// next clock for item's client.
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
