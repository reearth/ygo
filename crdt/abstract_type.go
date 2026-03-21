package crdt

// abstractType is the base embedded in every shared type (YArray, YMap, YText, …).
// It owns the doubly-linked list of Items that backs the type's content and
// provides the bookkeeping the Item integration algorithm needs.
//
// Being unexported keeps these internal pointers out of the public API while
// allowing YArray/YMap (also in this package) to embed it without an import cycle.
type abstractType struct {
	doc    *Doc
	start  *Item            // first item in the linked list (nil if empty)
	itemMap map[string]*Item // last item per key — non-nil only for map-based types
	length  int              // logical length (only countable, non-deleted items)
	item   *Item            // the Item that contains this type, when nested
}

// Observe subscriptions are stored here; expanded in Phase 3.
type observerHandle struct {
	fn func()
}
