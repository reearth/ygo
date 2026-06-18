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
	// ParentSub is the map key for YMap entries (and XML/Map-typed attributes).
	// nil means "no key" — a sequence element (YArray / YText / XML child).
	// A non-nil pointer to "" is a genuine empty-string key (m.Set("", v)),
	// which Yjs supports and which must be distinguished from nil. (#YMap-wire)
	ParentSub *string
	Content   Content
	Deleted   bool
	// parentID is transient decode state: the ID of the container item when this
	// item's parent is referenced by item-ID (a nested type) but that container
	// has not yet been integrated. It lets integration defer the item until the
	// parent arrives (Yjs pendingStructs parity, review finding C-3) instead of
	// hard-failing the decode. Resolved to Parent during integration; never encoded.
	parentID *ID
	// MovedBy points to the winning ContentMove item that has claimed this item
	// as its target. When non-nil, this item is rendered at the ContentMove's
	// position instead of its original linked-list position. Set by integrate()
	// during ContentMove priority arbitration.
	MovedBy *Item
	// redone is transient UndoManager state: when this item's deletion has been
	// undone, it holds the ID of the new item that re-inserted a copy of its
	// content. Undo restores a deletion by re-inserting (so the change
	// propagates to peers), not by flipping Deleted in place; redone tracks the
	// re-insertion to avoid redoing the same item twice and to let neighbour
	// position-tracing follow the chain. Never encoded. (Yjs Item.redone parity.)
	redone *ID
}

// parentSubKey collapses a *string parentSub to a plain string bucket label for
// changed-set tracking (addChanged). Both nil (a sequence element) and a
// pointer-to-"" (a genuine empty-string map key) collapse to "". This is safe
// because the result is only ever a label identifying which observer-event
// bucket is dirty — never an identity. Consumers that resolve a bucket back to
// items (e.g. YMap.computeKeys) re-filter on the actual ParentSub pointer
// (nil vs non-nil and its value), so a nil-keyed sequence element is never
// mistaken for an empty-string-keyed map entry. This holds even on XML
// elements, which legitimately mix keyed attributes (non-nil ParentSub) and
// nil-keyed children in the same type.
func parentSubKey(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// parentSubEqual reports whether two parentSub pointers denote the same key:
// both nil, or both non-nil with equal value.
func parentSubEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// strPtr returns a pointer to a copy of s — used when setting a map key so the
// stored pointer is stable and independent of the caller's variable.
func strPtr(s string) *string { return &s }

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

	// Resolve the right boundary from OriginRight (Yjs/yrs parity). The
	// conflict-scan loop below terminates on `o != item.Right`, so without
	// this resolution the loop has no upper bound and can place the item
	// past concurrent items that share the same Origin — see issue #65 and
	// the broader OriginRight gaps in #68. Mirrors Yjs JS's
	// `if (this.rightOrigin !== null) this.right = getItemCleanStart(...)`
	// at the top of Item.integrate.
	if item.Right == nil && item.OriginRight != nil {
		item.Right = txn.doc.store.getItemCleanStart(txn, *item.OriginRight)
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
					// Reuse the map instead of reallocating (Yjs does
					// conflictingItems.clear() here). Under high same-position
					// contention this fires O(group size) times per integrate, so
					// a fresh make() each time made conflict-scan allocation
					// quadratic in the conflict-group size (#54-C).
					clear(conflicting)
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
						clear(conflicting) // reuse the map, not realloc (see above / Yjs parity)
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
		// New head — the firstLive memoisation may point past us; reset it
		// so the next cleanup walk picks up the new live item (#86).
		item.Parent.invalidateFirstLiveCache()
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

	// ContentMove priority arbitration: resolve the target item (splitting if
	// needed so it covers exactly TargetLen elements) and claim it if we are the
	// winning move. Lower ClientID wins for concurrent moves from different peers;
	// for same-client sequential moves the earlier (lower-clock) move stays as
	// winner so that re-moves are not silently ignored — callers should delete
	// the old ContentMove first when they want to supersede it.
	if cm, ok := item.Content.(*ContentMove); ok && !item.Deleted && cm.Target != nil {
		target := resolveMovedItem(txn, cm.Target, cm.TargetLen)
		if target != nil {
			if target.MovedBy == nil || item.ID.Client < target.MovedBy.ID.Client {
				target.MovedBy = item
			}
		}
	}

	// Track ContentString items for end-of-transaction run squashing.
	if _, ok := item.Content.(*ContentString); ok {
		txn.newItems = append(txn.newItems, item)
	}

	// Yjs parity: once any ContentFormat is integrated into this type, mark
	// hasFormatting so subsequent YText.Delete calls know they need to run
	// the contextless cleanup walk. Plain-text types never set this flag,
	// so their deletes skip the walk entirely (#86 final fix).
	if _, ok := item.Content.(*ContentFormat); ok && item.Parent != nil {
		item.Parent.hasFormatting = true
	}

	// If this item wraps a nested type, set the back-pointer so the type
	// can identify its containing item during update encoding.
	if ct, ok := item.Content.(*ContentType); ok {
		ct.Type.item = item
	}

	// For map-keyed items, maintain last-write-wins semantics. The winner for a
	// key is the RIGHTMOST same-key item in YATA order (Yjs's parent._map value).
	// YATA placement above already orders concurrent same-key writes
	// deterministically by (origin, clientID), so this resolution is independent
	// of arrival order. We must scan past items of OTHER keys (and tombstones) to
	// the right rather than inspecting only the immediate right neighbour: a
	// different-key item landing between two same-key items would otherwise make
	// us falsely believe we are the rightmost, permanently diverging the winner
	// and losing the key on cross-sync (review finding C-1).
	if item.ParentSub != nil {
		key := *item.ParentSub
		rightmost := true
		// Fast path: if no item has ever been recorded for this key, none can be
		// to our right, so we are trivially the rightmost. This keeps populating a
		// map with N distinct keys O(N) rather than O(N²). Only when a prior
		// same-key item exists do we scan right (past other keys / tombstones) to
		// see whether it sits to our right and supersedes us.
		if _, exists := item.Parent.itemMap[key]; exists {
			for r := item.Right; r != nil; r = r.Right {
				if parentSubEqual(r.ParentSub, item.ParentSub) {
					// A same-key item (live or tombstone) sits to our right and is
					// therefore the more-recent value — we are superseded.
					rightmost = false
					break
				}
			}
		}
		if !rightmost {
			item.delete(txn)
		} else {
			// We are the rightmost item for this key; delete the previous winner.
			if existing, ok := item.Parent.itemMap[key]; ok && existing != item && !existing.Deleted {
				existing.delete(txn)
			}
			item.Parent.itemMap[key] = item
		}
	}

	if item.Parent != nil {
		txn.addChanged(item.Parent, parentSubKey(item.ParentSub))
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
//
// Cascade: when this item wraps a ContentType (nested YMap/YArray/YText/…),
// every child item is recursively deleted so the delete-set encoded on the
// wire includes the children's clocks. Without this, peers that held the
// same nested type would see inner items as live after the outer container
// was deleted (Yjs JS Item.delete walks content.getContent() identically;
// yrs Block::delete does the same). See #72 vector B1.
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
		txn.addChanged(item.Parent, parentSubKey(item.ParentSub))
	}

	// Recurse into nested-type children so their clocks land in the
	// delete-set too. Recursive call handles arbitrarily-deep nesting.
	if ct, ok := item.Content.(*ContentType); ok && ct.Type != nil {
		for child := ct.Type.start; child != nil; child = child.Right {
			if !child.Deleted {
				child.delete(txn)
			}
		}
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
	// #78 H2 — track the right half so tryMergeWithLefts can reverse the split
	// at transaction commit if no item ends up inserted between the two halves.
	// txn is guaranteed non-nil here (insertItem above would have panicked).
	txn.mergeStructs = append(txn.mergeStructs, right)
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

// resolveMovedItem finds the item at targetID and ensures it covers exactly
// targetLen clock units starting at targetID.Clock. When the item is part of
// a larger multi-value ContentAny (common when a delta update arrives without
// the local splits a sender performed during Move()), splitItem is called to
// carve out the exact boundary. Returns nil when no item contains targetID.
func resolveMovedItem(txn *Transaction, targetID *ID, targetLen int) *Item {
	si := txn.doc.store.Find(*targetID)
	if si == nil {
		return nil
	}
	// Trim leading prefix: ensure si starts exactly at targetID.Clock.
	if si.ID.Clock < targetID.Clock {
		prefix := int(targetID.Clock - si.ID.Clock)
		si = splitItem(txn, si, prefix) // si is now the right half
	}
	// Trim trailing suffix: ensure si covers no more than targetLen units.
	if targetLen > 0 && si.Content.Len() > targetLen {
		splitItem(txn, si, targetLen)
		// si (the left half) now has exactly targetLen units.
	}
	return si
}
