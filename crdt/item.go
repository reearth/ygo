package crdt

// Item is the fundamental unit of the Yjs CRDT. Every insertion creates
// one Item. Items form a doubly-linked list inside each shared type and are
// never removed — deleted items become tombstones (Deleted = true, content
// replaced by ContentDeleted when GC runs).
type Item struct {
	ID          ID
	Origin      *ID   // ID of the left neighbour at insertion time; nil = inserted at start
	OriginRight *ID   // ID of the right neighbour at insertion time; nil = inserted at end
	Left        *Item // current left neighbour in the linked list
	Right       *Item // current right neighbour
	Parent      *abstractType
	ParentSub   string // non-empty for YMap entries (the map key)
	Content     Content
	Deleted     bool
}

// integrate inserts this item into its parent's linked list using the YATA
// conflict-resolution algorithm. After integrate returns, Left and Right
// reflect the item's final position.
//
// offset > 0 is only needed when the item partially overlaps an existing item
// in the store (a split scenario during update decoding). For Phase 2 all
// items arrive cleanly (offset = 0).
func (item *Item) integrate(txn *Transaction, offset int) {
	if offset > 0 {
		item.ID.Clock += uint64(offset)
		item.Left = txn.doc.store.getItemCleanEnd(txn, item.ID.Client, item.ID.Clock-1)
		if item.Left != nil {
			last := item.Left.ID.Clock + uint64(item.Left.Content.Len()) - 1
			item.Origin = &ID{Client: item.Left.ID.Client, Clock: last}
		}
		item.Content = item.Content.Splice(offset)
	}

	if item.Parent == nil {
		return
	}

	// Determine the starting scan position: immediately right of the left origin.
	left := item.Left
	var o *Item
	if left == nil {
		o = item.Parent.start
	} else {
		o = left.Right
	}

	// Fast path: no conflict scanning needed when there are no items between
	// the left origin and the right origin. This is the common case for local
	// inserts at the end of a run and for remote items decoded in clock order.
	if o != nil && o != item.Right {
		// Slow path: conflicting is the set of items in the current conflict
		// group (items with the same left origin as us that we are comparing
		// against). beforeOrigin tracks every item we have scanned past, so we
		// can detect whether a later item's origin lies inside the conflict zone.
		//
		// Both maps are allocated here rather than unconditionally so that the
		// common (no-conflict) case pays zero allocation cost.
		conflicting := make(map[*Item]struct{})
		beforeOrigin := make(map[*Item]struct{})

		// Scan right until we hit our right origin (item.Right) or the end.
		for o != nil && o != item.Right {
			beforeOrigin[o] = struct{}{}
			conflicting[o] = struct{}{}

			if originIDEquals(item.Origin, o.Origin) {
				// Case 1: o has the same left origin as us — concurrent insert at
				// the same position. Lower ClientID wins (placed to the left).
				if o.ID.Client < item.ID.Client {
					left = o
					conflicting = make(map[*Item]struct{})
				} else if originIDEquals(item.OriginRight, o.OriginRight) {
					// Same left and right origin — truly symmetric; stop.
					break
				}
			} else if o.Origin != nil {
				// Case 2: o has a different left origin. Check whether that
				// origin lies before the conflict zone (beforeOrigin) or within
				// it (conflicting). If inside, o belongs after us — skip it.
				oOriginItem := txn.doc.store.Find(*o.Origin)
				if oOriginItem == nil {
					break
				}
				if _, inBefore := beforeOrigin[oOriginItem]; inBefore {
					if _, inConflict := conflicting[oOriginItem]; !inConflict {
						left = o
						conflicting = make(map[*Item]struct{})
					}
				} else {
					break
				}
			} else {
				break
			}

			o = o.Right
		}
	}

	// Insert item between left and left.Right.
	item.Left = left
	if left == nil {
		item.Right = item.Parent.start
		item.Parent.start = item
	} else {
		item.Right = left.Right
		left.Right = item
	}
	// Back-pointer: if our right neighbour exists, point it back to us.
	if item.Right != nil {
		item.Right.Left = item
	}

	// Update logical length and, if necessary, invalidate the position cache.
	// When the item is appended at the end (item.Right == nil), all existing
	// cache entries remain valid — no previously-cached position shifts.
	// For middle insertions we must discard cache entries at and after the
	// insertion point. When the caller set insertHint (local inserts where the
	// logical index is known) we do a partial clear, preserving entries before
	// the hint position so the next nearby lookup can resume from a cache hit
	// rather than rescanning from t.start. For remote updates (no hint) we fall
	// back to a full clear.
	if !item.Deleted && item.Content.IsCountable() {
		item.Parent.length += item.Content.Len()
		if item.Right != nil {
			if hint := item.Parent.insertHint; hint > 0 {
				item.Parent.insertHint = 0
				item.Parent.invalidatePosCacheFrom(hint)
			} else {
				item.Parent.invalidatePosCache()
			}
		} else {
			item.Parent.insertHint = 0 // reset even on end-append (no cache action needed)
		}
	}

	// Register in the document store.
	txn.doc.store.Append(item)

	// Track ContentString items for end-of-transaction run squashing.
	if _, ok := item.Content.(*ContentString); ok {
		txn.newItems = append(txn.newItems, item)
	}

	// If this item wraps a nested type, set the back-pointer so the type
	// can identify its containing item during update encoding.
	if ct, ok := item.Content.(*ContentType); ok {
		ct.Type.item = item
	}

	// For map-keyed items, maintain last-write-wins semantics.
	if item.ParentSub != "" {
		if item.Right != nil && item.Right.ParentSub == item.ParentSub {
			// A same-key item to our right won the concurrent write race — delete ourselves.
			item.delete(txn)
		} else {
			// We are the rightmost item for this key; delete the previous winner.
			if existing, ok := item.Parent.itemMap[item.ParentSub]; ok && !existing.Deleted {
				existing.delete(txn)
			}
			item.Parent.itemMap[item.ParentSub] = item
		}
	}

	if item.Parent != nil {
		txn.addChanged(item.Parent, item.ParentSub)
	}
}

// delete marks this item as a tombstone. The item stays in the linked list so
// that position references from other items (via Origin) remain valid.
//
// Cache invalidation strategy: for remote transactions (txn.Local == false)
// we clear the entire posCache here because the caller doesn't know the
// logical position. For local transactions the caller (e.g., deleteRange)
// is responsible for calling invalidatePosCacheFrom before scanning, so we
// skip the redundant full clear to avoid O(n²) behaviour.
func (item *Item) delete(txn *Transaction) {
	if item.Deleted {
		return
	}
	item.Deleted = true
	if item.Parent != nil && item.Content.IsCountable() {
		item.Parent.length -= item.Content.Len()
		if !txn.Local {
			item.Parent.invalidatePosCache()
		}
	}
	txn.deleteSet.add(item.ID, item.Content.Len())
	if item.Parent != nil {
		txn.addChanged(item.Parent, item.ParentSub)
	}
}

// splitItem splits item at offset, returning the new right half.
// item.Content is mutated to hold [0, offset); the returned item holds [offset, end).
// Both halves are registered in the store. The linked-list pointers are updated.
func splitItem(txn *Transaction, item *Item, offset int) *Item {
	rightContent := item.Content.Splice(offset) // mutates item.Content → [0, offset)
	right := &Item{
		ID:          ID{Client: item.ID.Client, Clock: item.ID.Clock + uint64(offset)},
		Origin:      &ID{Client: item.ID.Client, Clock: item.ID.Clock + uint64(offset) - 1},
		OriginRight: item.OriginRight,
		Left:        item,
		Right:       item.Right,
		Parent:      item.Parent,
		ParentSub:   item.ParentSub,
		Content:     rightContent,
		Deleted:     item.Deleted,
	}
	if right.Right != nil {
		right.Right.Left = right
	}
	item.Right = right
	txn.doc.store.insertItem(right)
	// The split shortens item's content, invalidating any cached boundary that
	// pointed to item's old end position. Clear the entire position cache so
	// subsequent leftNeighbourAt calls re-scan rather than using stale entries.
	if item.Parent != nil {
		item.Parent.invalidatePosCache()
	}
	return right
}

// originIDEquals compares two nullable ID pointers.
func originIDEquals(a, b *ID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Client == b.Client && a.Clock == b.Clock
}
