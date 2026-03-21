package crdt

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
	doc     *Doc
	start   *Item
	itemMap map[string]*Item // last live item per key; non-nil only for map-based types
	length  int              // logical length (non-deleted, countable items only)
	item    *Item            // the Item containing this type when nested
	owner   sharedType       // back-pointer to the concrete wrapper
	name    string           // root type name; used during V1 update encoding
}

// leftNeighbourAt returns the item that should be the left neighbour when
// inserting at logical position index, plus the offset within that item.
//
// If offset == 0, the insertion point is right after the returned item.
// If offset > 0, the insertion point is inside the returned item and the
// caller must split it before inserting.
// Returns (nil, 0) when index == 0 (insert at the very beginning).
func (t *abstractType) leftNeighbourAt(index int) (*Item, int) {
	if index == 0 {
		return nil, 0
	}
	counted := 0
	var lastItem *Item
	for item := t.start; item != nil; item = item.Right {
		if !item.Deleted && item.Content.IsCountable() {
			n := item.Content.Len()
			if counted+n >= index {
				offset := index - counted
				if offset == n {
					return item, 0
				}
				return item, offset
			}
			counted += n
			lastItem = item
		}
	}
	// index >= length: insert after the last item (append).
	return lastItem, 0
}
