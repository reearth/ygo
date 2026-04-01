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

// deepSub pairs a unique subscription ID with an ObserveDeep callback.
// The ID-based design allows out-of-order unsubscription without the
// index-capture bug that affects slice-index closures.
type deepSub struct {
	id uint64
	fn func(*Transaction)
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
	// deepSubIDGen issues unique IDs for ObserveDeep subscriptions so that
	// out-of-order unsubscription removes the correct entry (C5).
	deepSubIDGen  uint64
	deepObservers []deepSub

	// posCache is a small circular cache of (cumulativeIndex → *Item) pairs
	// used by leftNeighbourAt to skip linear scan from t.start on repeat
	// accesses. posCacheLen tracks how many slots are filled (capped at
	// posLRUSize). posCacheWr is the next write position; it wraps around once
	// the cache is full, giving O(1) FIFO eviction instead of the previous
	// O(posLRUSize) min-scan.
	posCache    [posLRUSize]posCacheEntry
	posCacheLen int
	posCacheWr  int

	// insertHint is set by Insert callers to the logical index of an imminent
	// local insertion. When non-zero, item.integrate uses partial cache
	// invalidation (discarding only entries ≥ insertHint) instead of clearing
	// the entire cache, so that entries before the insertion point survive for
	// subsequent nearby lookups. Zero means "no hint; do a full clear".
	insertHint int
}

// invalidatePosCache clears all cached position entries. Must be called
// whenever an insertion or deletion changes the logical positions of items
// in this type's linked list.
func (t *abstractType) invalidatePosCache() {
	t.posCacheLen = 0
	t.posCacheWr = 0
}

// invalidatePosCacheFrom removes all cached entries with cumulative index ≥ pos.
// Entries before pos remain valid and can be reused by the next leftNeighbourAt
// call near the same location, avoiding a full O(n) rescan from t.start.
func (t *abstractType) invalidatePosCacheFrom(pos int) {
	n := 0
	for i := 0; i < t.posCacheLen; i++ {
		if t.posCache[i].index < pos {
			t.posCache[n] = t.posCache[i]
			n++
		}
	}
	t.posCacheLen = n
	t.posCacheWr = 0 // reset write cursor; the compacted entries sit at [0..n-1]
}

// storePosCache records the entry (index, item) in the circular cache.
// When the cache is not yet full entries are appended; once full the oldest
// entry is overwritten in FIFO order. This gives O(1) insertion cost vs the
// previous O(posLRUSize) min-scan eviction strategy.
func (t *abstractType) storePosCache(index int, item *Item) {
	if t.posCacheLen < posLRUSize {
		t.posCache[t.posCacheLen] = posCacheEntry{index, item}
		t.posCacheLen++
		return
	}
	// Cache full: circular overwrite.
	t.posCache[t.posCacheWr] = posCacheEntry{index, item}
	t.posCacheWr++
	if t.posCacheWr >= posLRUSize {
		t.posCacheWr = 0
	}
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

// observeDeep registers fn to be called after any transaction that modifies
// this type or any nested shared type within it. Returns an unsubscribe
// function. Uses an ID-based lookup so out-of-order unsubscription is safe.
func (t *abstractType) observeDeep(fn func(*Transaction)) func() {
	t.deepSubIDGen++
	id := t.deepSubIDGen
	t.deepObservers = append(t.deepObservers, deepSub{id: id, fn: fn})
	return func() {
		for i, s := range t.deepObservers {
			if s.id == id {
				t.deepObservers = append(t.deepObservers[:i], t.deepObservers[i+1:]...)
				return
			}
		}
	}
}
