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

// findMarkerMut is the mutating write-path counterpart of findMarkerRO. It
// returns the same oracle-correct (item, renderedStart) answer, but as a side
// effect it maintains t.markers: it refreshes the nearest marker in place when
// the walk to the target was short, otherwise records a new marker (evicting
// the oldest by timestamp once maxSearchMarker markers exist). It is the ONLY
// place that writes to t.markers/t.markerTimestamp during normal operation, so
// it must be called only under the document write lock. findMarkerRO must stay
// read-only.
//
// Ports Yjs AbstractType.findMarker (src/types/AbstractType.js): locate the
// nearest existing marker, walk right/left to the item containing index, then
// walk left until that item can't merge with its left neighbour (same client &&
// contiguous clock) so a marker never points at the middle of a mergeable run.
func (t *abstractType) findMarkerMut(index int) (*Item, int) {
	if index > 0 && !t.disableMarkers && t.start != nil {
		t.markPositionAt(index)
	}
	// The answer itself is always produced by the read-only lookup, which is
	// proven equal to the cold oracle for ANY (accurate) marker configuration
	// — including the one markPositionAt just installed. Deriving the return
	// value this way keeps the exact-boundary tie-breaking (returning the
	// earlier item when index lands on an item boundary) in a single place.
	return t.findMarkerRO(index)
}

// markPositionAt performs the Yjs findMarker walk purely for its side effect:
// installing/refreshing a search marker at the canonical (non-mid-run) item
// that contains rendered position index. It never returns the lookup result
// (findMarkerMut derives that from findMarkerRO) and must run under the write
// lock. Precondition: index > 0, !t.disableMarkers, t.start != nil.
func (t *abstractType) markPositionAt(index int) {
	// Nearest existing marker by |m.index - index| (linear scan, ≤ maxSearchMarker).
	var marker *searchMarker
	bestDist := 0
	for i := range t.markers {
		if t.markers[i].item == nil {
			continue
		}
		d := t.markers[i].index - index
		if d < 0 {
			d = -d
		}
		if marker == nil || d < bestDist {
			marker = &t.markers[i]
			bestDist = d
		}
	}

	p := t.start
	pindex := 0
	if marker != nil {
		p = marker.item
		pindex = marker.index
		// We are using this marker; bump its recency so LRU eviction prefers
		// genuinely stale entries.
		t.markerTimestamp++
		marker.timestamp = t.markerTimestamp
	}

	// Iterate right while the running index is still left of the target.
	for p.Right != nil && pindex < index {
		if !p.Deleted && p.Content.IsCountable() {
			if index < pindex+p.Content.Len() {
				break
			}
			pindex += p.Content.Len()
		}
		p = p.Right
	}
	// Iterate left if we overshot (marker started to the right of the target).
	for p.Left != nil && pindex > index {
		p = p.Left
		if !p.Deleted && p.Content.IsCountable() {
			pindex -= p.Content.Len()
		}
	}
	// Iterate left until p can't merge with its left neighbour, so the marker
	// never points at the middle of a run that a later merge would collapse.
	for p.Left != nil &&
		p.Left.ID.Client == p.ID.Client &&
		p.Left.ID.Clock+uint64(p.Left.Content.Len()) == p.ID.Clock {
		p = p.Left
		if !p.Deleted && p.Content.IsCountable() {
			pindex -= p.Content.Len()
		}
	}

	// Refresh the nearest marker in place when the walk was short (Yjs uses
	// float division here: with a document shorter than maxSearchMarker the
	// threshold is < 1, so an existing marker is reused only when the walk
	// landed exactly on it); otherwise install a fresh marker.
	if marker != nil && float64(absInt(marker.index-pindex)) < float64(t.length)/float64(maxSearchMarker) {
		marker.item = p
		marker.index = pindex
		t.markerTimestamp++
		marker.timestamp = t.markerTimestamp
		return
	}
	t.markPosition(p, pindex)
}

// markPosition records a new search marker for item at rendered position
// index. When the cache is full it overwrites the marker with the oldest
// timestamp (LRU eviction), matching Yjs markPosition. Write-lock only.
func (t *abstractType) markPosition(item *Item, index int) {
	t.markerTimestamp++
	m := searchMarker{item: item, index: index, timestamp: t.markerTimestamp}
	if len(t.markers) < maxSearchMarker {
		t.markers = append(t.markers, m)
		return
	}
	oldest := 0
	for i := 1; i < len(t.markers); i++ {
		if t.markers[i].timestamp < t.markers[oldest].timestamp {
			oldest = i
		}
	}
	t.markers[oldest] = m
}

// updateMarkerChanges shifts marker indices to account for an edit of size
// delta (delta > 0 insert, delta < 0 delete) that begins at rendered position
// index, and drops markers whose item has become deleted/invalid. It is the
// maintenance primitive wired into the local structural mutation sites (Task 3):
// a positioned insert calls it with (index, +len), a positioned delete with
// (index, -len) AFTER tombstoning the affected items. Write-lock only.
//
// Shift condition — index <= m.index. Every marker whose recorded rendered
// start is at or after the edit position shifts by delta; markers strictly
// before the edit are untouched (an edit at index never moves the rendered
// start of an item that ends at or before index). This is the "index <= m.index"
// form the Yjs source notes "would actually suffice" (src/types/AbstractType.js).
//
// Why "<=" and not the "index < m.index || (delta<0 && index==m.index)" that a
// literal port of Yjs's default branch uses: Yjs additionally marks items with a
// boolean and de-dups a marker that lands on an already-marked item, so a marker
// left un-shifted at the exact boundary is harmless there. Our findMarkerRO has
// no such de-dup — it TRUSTS m.index as m.item's rendered start and re-walks from
// it — so a marker sitting exactly at an insert position MUST shift, or it would
// report a stale (too-low) start for the item that the insert pushed right. For
// deletes the two forms coincide (the boundary marker's item is tombstoned and
// dropped by the check above), so "<=" is correct for both directions.
//
// Post-condition (the invariant every caller relies on): each surviving marker
// still satisfies renderedStart(m.item) == m.index, given the list and Deleted
// flags are already updated for this edit.
func (t *abstractType) updateMarkerChanges(index, delta int) {
	for i := len(t.markers) - 1; i >= 0; i-- {
		m := &t.markers[i]
		if m.item == nil || m.item.Deleted {
			t.markers = append(t.markers[:i], t.markers[i+1:]...)
			continue
		}
		if index <= m.index {
			ni := m.index + delta
			if ni < index {
				ni = index
			}
			m.index = ni
		}
	}
}

// clearMarkers drops every search marker. Always safe: a subsequent lookup
// simply falls back to a cold walk and repopulates markers. markerTimestamp is
// intentionally left monotonic across clears.
func (t *abstractType) clearMarkers() {
	if len(t.markers) > 0 {
		t.markers = t.markers[:0]
	}
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
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
