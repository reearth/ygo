package crdt

// maxSearchMarker is the maximum number of search markers cached per
// abstractType, matching the value used by the Yjs reference implementation
// (ArraySearchMarker). A small fixed cap keeps the nearest-marker scan O(1)
// while still giving good hit rates for typical access patterns (sequential
// edits, cursor-local edits, etc).
const maxSearchMarker = 80

// searchMarker pins a rendered position (index) to the item that starts at
// that position, plus a timestamp used by later tasks for LRU-style
// eviction/refresh policy. Task 1 only defines the struct and a read-only
// lookup; nothing yet writes to t.markers (that lands in later tasks that
// replace posCache).
type searchMarker struct {
	item      *Item
	index     int
	timestamp uint64
}

// renderedStep reports how a single item contributes to rendered (visible)
// position. This is the single shared definition of position-contribution
// used by marker-based lookups.
//
// Task 1 ships the non-move version: a deleted item contributes nothing, and
// a countable, non-deleted item contributes its Content.Len(); everything
// else (formatting marks, moves, non-countable content) contributes 0.
// renderAt is reserved for move-awareness (Task 5), where a moved item's
// rendered contribution is attributed to a different item than the one being
// stepped over; until then it is always nil.
func (t *abstractType) renderedStep(item *Item) (countable bool, n int, renderAt *Item) {
	if item == nil || item.Deleted || !item.Content.IsCountable() {
		return false, 0, nil
	}
	return true, item.Content.Len(), nil
}

// findMarkerRO returns the item spanning the rendered position index, along
// with the rendered start index of that item (the cumulative rendered length
// of everything strictly before it). It is read-only: it never writes to
// t.markers, t.markerTimestamp, or any other field — later tasks add
// concurrent-reader tests that call this under an RLock, so mutating the
// marker cache here would be a data race.
//
// Returns (nil, 0) for index<=0 or an empty type.
//
// When t.markers is empty or t.disableMarkers is set (the force-cold test
// seam), this walks from t.start exactly like the marker-free oracle. When
// markers are present, it starts from the nearest existing marker (by
// |marker.index-index|, a linear scan over at most maxSearchMarker entries)
// and walks right or left from there via renderedStep, accumulating rendered
// length until the target index is bracketed.
func (t *abstractType) findMarkerRO(index int) (*Item, int) {
	if index <= 0 {
		return nil, 0
	}
	if t.disableMarkers || len(t.markers) == 0 {
		return t.walkColdFrom(t.start, 0, index)
	}

	// Find the nearest existing marker by |marker.index - index|.
	best := -1
	bestDist := 0
	for i, m := range t.markers {
		if m.item == nil {
			continue
		}
		dist := m.index - index
		if dist < 0 {
			dist = -dist
		}
		if best == -1 || dist < bestDist {
			best = i
			bestDist = dist
		}
	}
	if best == -1 {
		return t.walkColdFrom(t.start, 0, index)
	}

	m := t.markers[best]
	// The marker's item must itself be a valid (countable, non-deleted)
	// anchor — that is the only kind of item the oracle ever returns, so a
	// well-formed marker always points at one. If it doesn't (a stale
	// marker), fall back to a full cold walk rather than risk a wrong
	// answer.
	countable, n, _ := t.renderedStep(m.item)
	if !countable {
		return t.walkColdFrom(t.start, 0, index)
	}

	// end is the rendered index immediately after m.item (i.e. the start of
	// whatever the oracle's forward scan would look at next). Comparing
	// index against end — not against m.index — is what correctly captures
	// the oracle's left-to-right tie-breaking rule: when index lands
	// exactly on the boundary between two items, the oracle's forward scan
	// reaches (and returns) the EARLIER item first, because that item's
	// "counted+n >= index" check fires before the later item is ever
	// examined. See walkLeftFrom for why walking left handles that case
	// correctly with a single pass (no need to re-check further left after
	// stepping once past the tie).
	end := m.index + n
	if end >= index {
		return t.walkLeftFrom(m.item, m.index, index)
	}
	// end < index: the target is strictly after m.item; every item up to
	// and including m.item is guaranteed not to satisfy the oracle's
	// condition (rendered length is monotonically non-decreasing along the
	// list), so resuming the forward scan at m.item.Right with counted=end
	// exactly replicates continuing the oracle's loop from t.start.
	return t.walkColdFrom(m.item.Right, end, index)
}

// walkColdFrom walks Right starting at "from" (which starts at rendered
// position "counted"), accumulating rendered length via renderedStep until
// the target index is bracketed. This is exactly the oracle's algorithm,
// just allowed to resume mid-list from an already-known-correct starting
// point instead of always starting at t.start. It never writes to t.markers.
func (t *abstractType) walkColdFrom(from *Item, counted int, index int) (*Item, int) {
	for item := from; item != nil; item = item.Right {
		countable, n, _ := t.renderedStep(item)
		if !countable {
			continue
		}
		if counted+n >= index {
			return item, counted
		}
		counted += n
	}
	return nil, counted
}

// walkLeftFrom returns the leftmost (earliest, in list order) countable
// non-deleted item that brackets index, given a starting candidate "cur"
// already known to satisfy end(cur) = curS + len(cur) >= index (curS is
// cur's own rendered start index, i.e. renderedStart(cur) == curS). It never
// writes to t.markers.
//
// Because every countable, non-deleted item has length >= 1, rendered start
// indices strictly increase from one countable item to the next, so at most
// one predecessor step is ever needed to resolve an exact-boundary tie
// (index == the current best's start): stepping to the immediately
// preceding countable item (skipping over any deleted/non-countable items,
// whose contribution is 0 and so don't shift the start index) always drops
// the start index strictly below index, terminating the walk. The general
// (non-tie, curS > index) case just repeats the same single-step logic
// until the current best's start index falls below index.
func (t *abstractType) walkLeftFrom(cur *Item, curS int, index int) (*Item, int) {
	best, bestS := cur, curS
	for bestS >= index {
		p := best.Left
		var pItem *Item
		var pLen int
		for p != nil {
			countable, n, _ := t.renderedStep(p)
			if countable {
				pItem = p
				pLen = n
				break
			}
			p = p.Left
		}
		if pItem == nil {
			// No earlier countable item exists (best is the first countable
			// item in the type) — best is the answer.
			break
		}
		best = pItem
		bestS -= pLen
	}
	return best, bestS
}
