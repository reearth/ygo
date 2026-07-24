package crdt

// SharedType is the public interface satisfied by every exported CRDT type
// (YArray, YMap, YText, YXmlFragment, …). It exists so external callers can
// name the element type of NewUndoManager's scope slice
// (`[]crdt.SharedType{txt, arr}`). Its methods are unexported, so only ygo's own
// types can satisfy it — external code can pass and hold values, but not
// implement it. It is a type alias for the internal sharedType, so existing
// in-package call sites are unaffected.
type SharedType = sharedType

// sharedType is implemented by every exported CRDT type (YArray, YMap, YText).
// Doc.share stores sharedType values so it can fire per-type observers after
// each transaction without knowing the concrete type.
type sharedType interface {
	baseType() *abstractType
	// prepareFire is called inside the document write lock in Transact.
	// It snapshots the current observer slice and builds the event struct,
	// then returns a closure that calls all snapshotted observers. The closure
	// is invoked after the lock is released, so observers may safely call back
	// into any Doc method. Returning nil means there are no observers to fire.
	// This pattern eliminates the data race between concurrent Observe() calls
	// and the observer fire loop (N-C1).
	prepareFire(txn *Transaction, keysChanged map[string]struct{}) func()
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
	doc     *Doc
	start   *Item
	itemMap map[string]*Item // last live item per key; non-nil only for map-based types
	length  int              // logical length (non-deleted, countable items only)
	item    *Item            // the Item containing this type when nested
	owner   sharedType       // back-pointer to the concrete wrapper
	name    string           // root type name; used during V1 update encoding
	// deepSubIDGen issues unique IDs for ObserveDeep subscriptions so that
	// out-of-order unsubscription removes the correct entry (C5).
	deepSubIDGen  uint64
	deepObservers []deepSub

	// insertHint is set by Insert callers to the logical index of an imminent
	// local insertion. When non-zero, item.integrate uses partial marker
	// invalidation (dropping only markers ≥ insertHint) instead of clearing
	// all markers, so that markers before the insertion point survive for
	// subsequent nearby lookups. Zero means "no hint; do a full clear".
	insertHint int

	// firstLiveCache memoises the first live (non-deleted) item from t.start.
	// Used by linked-list walks that would otherwise re-skip the same leading
	// tombstones on every call (deleteRange when many head-deletes accumulate,
	// per issue #86). Updated lazily by firstLiveFromStart; invalidated on
	// item.integrate when a new head replaces t.start. Because tombstoning
	// is monotonic, advancing the cache forward from its current value is
	// always safe — once we walk past a deleted item we never need to revisit.
	firstLiveCache *Item

	// hasFormatting becomes true the first time a ContentFormat item is
	// integrated into this type (locally via YText.Format or remotely via
	// an update). Mirrors Yjs's _hasFormatting flag. YText.Delete uses this
	// to skip the (expensive) per-deleted-item cleanup walk on types that
	// have never had formatting applied — the dominant cost on head-delete
	// workloads in plain-text documents. Once true, stays true.
	hasFormatting bool

	// markers is a small cache of (rendered index → *Item) search markers,
	// modelled on Yjs's ArraySearchMarker, replacing the old posCache. Capped
	// at maxSearchMarker entries. The write path (findMarkerMut, called from
	// leftNeighbourAt) is the only place that installs/refreshes/evicts
	// markers; the read-only findMarkerRO merely consults them. Because
	// mutation happens exclusively under the document write lock, findMarkerRO
	// stays safe to call under an RLock.
	markers []searchMarker
	// markerTimestamp is a monotonically increasing counter used to decide
	// which marker to evict/refresh (LRU-by-recency) in markPosition/
	// markPositionAt.
	markerTimestamp uint64
	// disableMarkers is a test-only seam that forces findMarkerRO (and, once
	// later tasks add a marker-aware write path, the rest of the
	// marker-based lookups) to fall back to a full linear walk from t.start,
	// exactly like the marker-free oracle. Used to assert marker-based and
	// cold-walk results agree.
	disableMarkers bool
}

// prelimFlusher is implemented by types that buffer mutations while detached
// (not yet part of a document tree) and replay them when their container item
// integrates — Yjs's "prelim content" semantics (_prelimContent/_prelimAttrs
// on YXmlFragment/YXmlElement, _pending on YText). item.integrate invokes it
// via the type's owner right after setting the container back-pointer, so
// buffered subtrees materialise top-down with parent-first clocks. (#yxml-wire)
type prelimFlusher interface {
	flushPrelim(txn *Transaction)
}

// detached reports whether this type is not (yet) part of a document tree:
// neither a named root type nor wrapped by an integrated container item.
// Mutations on detached types must be buffered (see prelimFlusher): creating
// items for them immediately would assign child clocks BELOW the future
// container item's clock — an ordering genuine Yjs never produces and cannot
// decode (Item.getMissing skips the missing-struct check for same-client
// parents, so Y.applyUpdate crashes on such an update). (#yxml-wire)
func (t *abstractType) detached() bool {
	return t.item == nil && t.name == ""
}

// firstLiveFromStart returns the first non-deleted item reachable from t.start
// by walking Right, or nil if every item is tombstoned. The result is memoised
// in t.firstLiveCache: subsequent calls advance the cache past any tombstones
// that accumulated since the last call rather than restarting from t.start.
//
// Cache invariant: t.firstLiveCache is either nil, the true first-live item,
// or an earlier item that may or may not still be live. The forward walk from
// the cache is always correct because tombstoning is monotonic — an item that
// is currently deleted stays deleted, so once we walk past it we never have
// to revisit it. The cache MUST be reset (to nil) only when a new item is
// inserted strictly before the cached pointer, which currently only happens
// when item.integrate replaces t.start (see item.go).
//
// Closes the O(N²) sequential-head-delete behaviour described in issue #86.
func (t *abstractType) firstLiveFromStart() *Item {
	node := t.firstLiveCache
	if node == nil {
		node = t.start
	}
	for node != nil && node.Deleted {
		node = node.Right
	}
	t.firstLiveCache = node
	return node
}

// invalidateFirstLiveCache clears the first-live memoisation. Callers must
// invoke this whenever a new item is inserted at the head of the linked list
// (i.e. as the new t.start) so the next firstLiveFromStart call resumes its
// walk from the new head rather than skipping past it.
func (t *abstractType) invalidateFirstLiveCache() {
	t.firstLiveCache = nil
}

// leftNeighbourAt returns the item that should be the left neighbour when
// inserting at logical position index, plus the offset within that item.
//
// If offset == 0, the insertion point is right after the returned item.
// If offset > 0, the insertion point is inside the returned item and the
// caller must split it before inserting.
// Returns (nil, 0) when index == 0 (insert at the very beginning).
//
// The search-marker cache (findMarkerMut) is consulted first so that repeated
// insertions near the same position avoid re-scanning from t.start; it also
// installs/refreshes a marker for the resolved position as a side effect.
func (t *abstractType) leftNeighbourAt(index int) (*Item, int) {
	if index == 0 {
		return nil, 0
	}

	item, start := t.findMarkerMut(index)
	if item == nil {
		// index > total rendered length (append / insert-beyond-end): anchor
		// after the last item that actually RENDERS, using the shared move-aware
		// renderedStep — NOT the raw `!Deleted && IsCountable` test the old
		// fallback used, which is move-blind. A winning ContentMove is
		// non-countable yet renders its target here (so it IS the rendered tail
		// and the correct physical anchor); a moved-away item is countable but
		// renders elsewhere (so it must NOT be chosen). Picking the physical
		// last-countable item instead would make append incoherent with the
		// move-aware Get/Slice/deleteRange: when an element is moved TO the end,
		// the ContentMove is the physical tail and the last plain item is not the
		// rendered tail, so the old walk anchored the new element BEFORE the moved
		// one (#181/#190). For move-free content renderedStep's countable branch
		// is exactly `!Deleted && IsCountable`, so this is byte-identical without
		// moves.
		var last *Item
		for it := t.start; it != nil; it = it.Right {
			if countable, _, _ := t.renderedStep(it); countable {
				last = it
			}
		}
		return last, 0
	}

	// findMarkerMut (like the cold oracle) always returns an item whose
	// rendered start is strictly < index, so offset ∈ [1, n]. offset == n
	// means index is exactly at the item's end → insert right after it;
	// otherwise the item must be split at offset. Use renderedStep's n (not
	// item.Content.Len()) for the same move-aware notion of "how wide is this
	// item" that findMarkerMut used to bracket index — keeping Insert's split
	// point consistent with the position walk. For a winning ContentMove n is
	// the moved target's width (==1, since Move forces single-element targets
	// and ContentMove.Len()==1), and for any plain item n==Content.Len(), so
	// this is a no-op relative to the old check; it only closes a latent gap if
	// the two notions of width could ever disagree for the returned item.
	_, n, _ := t.renderedStep(item)
	offset := index - start
	if offset >= n {
		return item, 0
	}
	return item, offset
}

// observeDeep registers fn to be called after any transaction that modifies
// this type or any nested shared type within it. Returns an unsubscribe
// function. Uses an ID-based lookup so out-of-order unsubscription is safe.
//
// Acquiring doc.mu.Lock() here serialises observer registration against
// Transact, which reads deepObservers under the same lock (N-C1).
func (t *abstractType) observeDeep(fn func(*Transaction)) func() {
	doc := t.doc
	if doc != nil {
		doc.mu.Lock()
		defer doc.mu.Unlock()
	}
	t.deepSubIDGen++
	id := t.deepSubIDGen
	t.deepObservers = append(t.deepObservers, deepSub{id: id, fn: fn})
	return func() {
		if doc := t.doc; doc != nil {
			doc.mu.Lock()
			defer doc.mu.Unlock()
		}
		for i, s := range t.deepObservers {
			if s.id == id {
				t.deepObservers = append(t.deepObservers[:i], t.deepObservers[i+1:]...)
				return
			}
		}
	}
}
