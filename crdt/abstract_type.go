package crdt

// posLRUSize is the number of (index → item) pairs cached per abstractType.
// 80 entries matches the value used by the Yjs reference implementation and
// gives O(1) average-case performance for sequential and nearby insertions.
const posLRUSize = 80

// posCacheEntry maps a logical cumulative character count to the item at that
// boundary. "index" is the total counted characters up to and including item.
type posCacheEntry struct {
	index int
	item  *Item
}

// sharedType is implemented by every exported CRDT type (YArray, YMap, YText).
// Doc.share stores sharedType values so it can fire per-type observers after
// each transaction without knowing the concrete type.
type sharedType interface {
	baseType() *abstractType
	fire(txn *Transaction, keysChanged map[string]struct{})
}

// abstractType is the base embedded in every shared type (YArray, YMap, YText).
// It owns the doubly-linked list of Items that backs the type's content and
// provides the bookkeeping that Item integration needs.
type abstractType struct {
	doc           *Doc
	start         *Item
	itemMap       map[string]*Item // last live item per key; non-nil only for map-based types
	length        int              // logical length (non-deleted, countable items only)
	item          *Item            // the Item containing this type when nested
	owner         sharedType       // back-pointer to the concrete wrapper
	name          string           // root type name; used during V1 update encoding
	deepObservers []func(*Transaction)

	// posCache is a small LRU cache of (cumulativeIndex → *Item) pairs used
	// by leftNeighbourAt to skip linear scan from t.start on repeat accesses.
	posCache    [posLRUSize]posCacheEntry
	posCacheLen int
}

// invalidatePosCache clears all cached position entries. Must be called
// whenever an insertion or deletion changes the logical positions of items
// in this type's linked list.
func (t *abstractType) invalidatePosCache() {
	t.posCacheLen = 0
}

// storePosCache records the entry (index, item) in the LRU cache. When the
// cache is full it evicts the entry with the smallest index, since forward
// sequential scans only benefit from entries ahead of the current position.
func (t *abstractType) storePosCache(index int, item *Item) {
	if t.posCacheLen < posLRUSize {
		t.posCache[t.posCacheLen] = posCacheEntry{index, item}
		t.posCacheLen++
		return
	}
	// Find and replace the minimum-index entry.
	minI, minIdx := 0, t.posCache[0].index
	for i := 1; i < posLRUSize; i++ {
		if t.posCache[i].index < minIdx {
			minI = i
			minIdx = t.posCache[i].index
		}
	}
	t.posCache[minI] = posCacheEntry{index, item}
}

// leftNeighbourAt returns the item that should be the left neighbour when
// inserting at logical position index, plus the offset within that item.
//
// If offset == 0, the insertion point is right after the returned item.
// If offset > 0, the insertion point is inside the returned item and the
// caller must split it before inserting.
// Returns (nil, 0) when index == 0 (insert at the very beginning).
//
// The LRU position cache is consulted first so that repeated insertions near
// the same position avoid re-scanning from t.start.
func (t *abstractType) leftNeighbourAt(index int) (*Item, int) {
	if index == 0 {
		return nil, 0
	}

	// Find the cache entry with the largest cumulative index ≤ requested index.
	// Deleted cached items are skipped (they are no longer at their recorded position).
	startCounted := 0
	var startItem *Item // first item to scan from (the Right of the cached boundary item)
	for i := 0; i < t.posCacheLen; i++ {
		e := t.posCache[i]
		if e.index <= index && e.index > startCounted && !e.item.Deleted {
			startCounted = e.index
			startItem = e.item
		}
	}

	counted := startCounted
	var lastItem *Item
	scanFrom := t.start
	if startItem != nil {
		// Resume scan from the item right after the cached boundary.
		lastItem = startItem
		scanFrom = startItem.Right
	}

	for item := scanFrom; item != nil; item = item.Right {
		if !item.Deleted && item.Content.IsCountable() {
			n := item.Content.Len()
			newCounted := counted + n
			// Store this boundary in the cache for future nearby lookups.
			t.storePosCache(newCounted, item)
			if newCounted >= index {
				offset := index - counted
				if offset == n {
					return item, 0
				}
				return item, offset
			}
			counted = newCounted
			lastItem = item
		}
	}
	// index >= length: insert after the last item (append).
	return lastItem, 0
}
