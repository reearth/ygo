package crdt

import "encoding/json"

// arraySub pairs a unique subscription ID with a YArrayEvent callback.
type arraySub struct {
	id uint64
	fn func(YArrayEvent)
}

// YArray is a shared ordered list that supports arbitrary-type elements.
// It embeds abstractType, which owns the underlying doubly-linked Item list.
type YArray struct {
	abstractType
	subIDGen  uint64
	observers []arraySub
}

func (a *YArray) baseType() *abstractType { return &a.abstractType }

// prepareFire snapshots the current observer slice inside the document write
// lock and returns a closure that fires all snapshotted observers. Callers in
// Transact invoke the returned closure after releasing the lock, so observers
// may safely call back into any Doc method (N-C1).
func (a *YArray) prepareFire(txn *Transaction, _ map[string]struct{}) func() {
	if len(a.observers) == 0 {
		return nil
	}
	snap := make([]arraySub, len(a.observers))
	copy(snap, a.observers)
	e := YArrayEvent{Target: a, Txn: txn}
	return func() {
		for _, s := range snap {
			s.fn(e)
		}
	}
}

// Len returns the number of non-deleted elements.
func (a *YArray) Len() int { return a.length }

// Insert inserts vals at logical position index (0 = prepend, Len() = append).
func (a *YArray) Insert(txn *Transaction, index int, vals []any) {
	t := &a.abstractType
	left, offset := t.leftNeighbourAt(index)
	if offset > 0 {
		splitItem(txn, left, offset)
		// left is now the left half; its Right points to the right half.
	}

	var origin *ID
	var originRight *ID
	if left != nil {
		end := left.ID.Clock + uint64(left.Content.Len()) - 1
		origin = &ID{Client: left.ID.Client, Clock: end}
		if left.Right != nil {
			id := left.Right.ID
			originRight = &id
		}
	} else if t.start != nil {
		id := t.start.ID
		originRight = &id
	}

	item := &Item{
		ID:          ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:      origin,
		OriginRight: originRight,
		Left:        left,
		Parent:      t,
		Content:     NewContentAny(vals...),
	}
	// Signal to integrate the logical index for partial cache invalidation.
	if index > 0 {
		t.insertHint = index
	}
	item.integrate(txn, 0)
}

// Push appends vals to the end of the array.
func (a *YArray) Push(txn *Transaction, vals []any) {
	a.Insert(txn, a.Len(), vals)
}

// Get returns the element at logical position index, or nil if out of bounds.
// Must not be called from inside a Transact callback — acquires a read lock
// that would deadlock with the write lock held by Transact.
func (a *YArray) Get(index int) any {
	if doc := a.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &a.abstractType
	counted := 0
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		if cm, ok := item.Content.(*ContentMove); ok {
			if a.doc != nil {
				target := a.doc.store.Find(*cm.Target)
				if target != nil && target.MovedBy == item && !target.Deleted {
					n := target.Content.Len()
					if counted+n > index {
						if ca, ok := target.Content.(*ContentAny); ok {
							return ca.Vals[index-counted]
						}
						return nil
					}
					counted += n
				}
			}
			continue
		}
		if !item.Content.IsCountable() {
			continue
		}
		if item.MovedBy != nil {
			continue
		}
		n := item.Content.Len()
		if counted+n > index {
			switch c := item.Content.(type) {
			case *ContentAny:
				return c.Vals[index-counted]
			case *ContentType:
				return c.Type.owner
			}
			return nil
		}
		counted += n
	}
	return nil
}

// Delete removes length elements starting at logical position index.
func (a *YArray) Delete(txn *Transaction, index, length int) {
	deleteRange(&a.abstractType, txn, index, length)
}

// ToSlice returns all non-deleted elements as a new slice.
// Must not be called from inside a Transact callback.
func (a *YArray) ToSlice() []any {
	if doc := a.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &a.abstractType
	result := make([]any, 0, t.length)
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		if cm, ok := item.Content.(*ContentMove); ok {
			// Render the moved item at this position if this move won.
			if a.doc != nil {
				target := a.doc.store.Find(*cm.Target)
				if target != nil && target.MovedBy == item && !target.Deleted {
					if ca, ok := target.Content.(*ContentAny); ok {
						result = append(result, ca.Vals...)
					}
				}
			}
			continue
		}
		if !item.Content.IsCountable() {
			continue
		}
		if item.MovedBy != nil {
			// Rendered at the ContentMove's position; skip here.
			continue
		}
		if ca, ok := item.Content.(*ContentAny); ok {
			result = append(result, ca.Vals...)
		}
	}
	return result
}

// ToJSON returns the array serialised as a JSON array.
// Must not be called from inside a Transact callback.
func (a *YArray) ToJSON() ([]byte, error) {
	return json.Marshal(a.ToSlice())
}

// Observe registers fn to be called after every transaction that modifies this
// array. Returns an unsubscribe function. Uses ID-based lookup so out-of-order
// unsubscription removes the correct entry (C5).
//
// Acquiring doc.mu.Lock() serialises registration against Transact, which
// reads the observer slice under the same lock (N-C1). Do not call Observe
// from inside a Transact callback — that would deadlock.
func (a *YArray) Observe(fn func(YArrayEvent)) func() {
	doc := a.doc
	if doc != nil {
		doc.mu.Lock()
		defer doc.mu.Unlock()
	}
	a.subIDGen++
	id := a.subIDGen
	a.observers = append(a.observers, arraySub{id: id, fn: fn})
	return func() {
		if doc := a.doc; doc != nil {
			doc.mu.Lock()
			defer doc.mu.Unlock()
		}
		for i, s := range a.observers {
			if s.id == id {
				a.observers = append(a.observers[:i], a.observers[i+1:]...)
				return
			}
		}
	}
}

// ObserveDeep registers fn to be called after any transaction that modifies
// this array or any nested shared type within it. Returns an unsubscribe function.
func (a *YArray) ObserveDeep(fn func(*Transaction)) func() {
	return a.observeDeep(fn)
}

// Slice returns elements in the half-open range [start, end).
// Clamps end to Len() if it exceeds the array length.
// Must not be called from inside a Transact callback.
func (a *YArray) Slice(start, end int) []any {
	if doc := a.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &a.abstractType
	if end > t.length {
		end = t.length
	}
	if start < 0 {
		start = 0
	}
	if start > end {
		return nil
	}
	result := make([]any, 0, end-start)
	counted := 0
	for item := t.start; item != nil && counted < end; item = item.Right {
		if item.Deleted {
			continue
		}
		if cm, ok := item.Content.(*ContentMove); ok {
			if a.doc != nil {
				target := a.doc.store.Find(*cm.Target)
				if target != nil && target.MovedBy == item && !target.Deleted {
					if ca, ok := target.Content.(*ContentAny); ok {
						for _, v := range ca.Vals {
							if counted >= start && counted < end {
								result = append(result, v)
							}
							counted++
							if counted >= end {
								break
							}
						}
					}
				}
			}
			continue
		}
		if !item.Content.IsCountable() {
			continue
		}
		if item.MovedBy != nil {
			continue
		}
		ca, ok := item.Content.(*ContentAny)
		if !ok {
			counted++
			continue
		}
		for _, v := range ca.Vals {
			if counted >= start && counted < end {
				result = append(result, v)
			}
			counted++
			if counted >= end {
				break
			}
		}
	}
	return result
}

// ForEach calls fn for every non-deleted element in index order.
// Must not be called from inside a Transact callback.
func (a *YArray) ForEach(fn func(index int, value any)) {
	if doc := a.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &a.abstractType
	index := 0
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		if cm, ok := item.Content.(*ContentMove); ok {
			if a.doc != nil {
				target := a.doc.store.Find(*cm.Target)
				if target != nil && target.MovedBy == item && !target.Deleted {
					if ca, ok := target.Content.(*ContentAny); ok {
						for _, v := range ca.Vals {
							fn(index, v)
							index++
						}
					}
				}
			}
			continue
		}
		if !item.Content.IsCountable() {
			continue
		}
		if item.MovedBy != nil {
			continue
		}
		if ca, ok := item.Content.(*ContentAny); ok {
			for _, v := range ca.Vals {
				fn(index, v)
				index++
			}
		}
	}
}

// Move relocates the element at fromIndex to toIndex in a CRDT-safe manner.
// Both indices are in terms of the logical (non-deleted) rendered position.
//
// Unlike the previous delete-then-insert implementation, Move now creates a
// ContentMove item at the destination position in the linked list. The original
// item remains in place (marked as moved via its MovedBy field) and is rendered
// at the ContentMove's position instead. This preserves causal history and
// converges correctly under concurrent edits:
//
//   - Two peers moving DIFFERENT elements: both moves apply, each element ends
//     up at its respective destination.
//   - Two peers moving THE SAME element: the ContentMove with the lower ClientID
//     wins; the element appears at the winner's destination.
//
// physPos formula: after splitting the target element into its own item, the
// ContentMove is placed at physical position toIndex+1 when fromIndex < toIndex
// (the target is still physically present and countable before being marked
// moved), or at toIndex when fromIndex > toIndex.
//
// Move walks the linked list directly rather than calling Get() to avoid the
// deadlock that would occur if RLock were acquired on top of the write lock held
// by the enclosing Transact callback.
func (a *YArray) Move(txn *Transaction, fromIndex, toIndex int) {
	if fromIndex == toIndex {
		return
	}
	t := &a.abstractType

	// Walk the rendered array to find the physical item at fromIndex.
	// ContentMove items are "expanded" in rendering order; items with MovedBy
	// set are skipped at their original position.
	counted := 0
	var targetItem *Item
	var targetOff int
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		if cm, ok := item.Content.(*ContentMove); ok {
			// ContentMove renders the target here if this move won.
			if a.doc != nil {
				target := a.doc.store.Find(*cm.Target)
				if target != nil && target.MovedBy == item && !target.Deleted {
					n := target.Content.Len()
					if counted+n > fromIndex {
						targetItem = target
						targetOff = fromIndex - counted
						break
					}
					counted += n
				}
			}
			continue
		}
		if !item.Content.IsCountable() || item.MovedBy != nil {
			continue
		}
		n := item.Content.Len()
		if counted+n > fromIndex {
			targetItem = item
			targetOff = fromIndex - counted
			break
		}
		counted += n
	}
	if targetItem == nil {
		return // out of bounds
	}

	// Isolate the single element at targetOff so it occupies its own item.
	if targetOff > 0 {
		targetItem = splitItem(txn, targetItem, targetOff)
	}
	if targetItem.Content.Len() > 1 {
		splitItem(txn, targetItem, 1)
	}

	// Compute physPos: the position in the PHYSICAL linked list (counting all
	// non-deleted IsCountable items, including those with MovedBy != nil) at which
	// the ContentMove item should be placed. After the move, the target item will
	// be skipped (MovedBy != nil) and the ContentMove will render it at physPos.
	//
	// fromIndex < toIndex: the target is at physical position fromIndex+1 or later
	// (since items before it are counted normally). physPos = toIndex+1 accounts for
	// the target still being countable at its original physical position.
	// fromIndex > toIndex: physPos = toIndex (the ContentMove slots in before the
	// item that is currently at toIndex in physical count).
	var physPos int
	if fromIndex < toIndex {
		physPos = toIndex + 1
	} else {
		physPos = toIndex
	}

	left, offset := t.leftNeighbourAt(physPos)
	if offset > 0 {
		splitItem(txn, left, offset)
		// After split, left holds the [0,offset) part; its Right is the new right half.
	}

	var origin *ID
	var originRight *ID
	if left != nil {
		end := left.ID.Clock + uint64(left.Content.Len()) - 1
		origin = &ID{Client: left.ID.Client, Clock: end}
		if left.Right != nil {
			id := left.Right.ID
			originRight = &id
		}
	} else if t.start != nil {
		id := t.start.ID
		originRight = &id
	}

	moveItem := &Item{
		ID:          ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:      origin,
		OriginRight: originRight,
		Left:        left,
		Parent:      t,
		Content:     NewContentMove(&targetItem.ID, targetItem.Content.Len()),
	}
	if toIndex > 0 {
		t.insertHint = toIndex
	}
	moveItem.integrate(txn, 0)
}

// deleteRange is shared by YArray and YText to delete a logical range.
func deleteRange(t *abstractType, txn *Transaction, index, length int) {
	if length <= 0 {
		return
	}
	// For local transactions, invalidate only the cache entries at and after the
	// deletion start. Entries before index remain valid and can be reused by a
	// subsequent leftNeighbourAt call near the same location.
	// For remote transactions, item.delete handles cache invalidation.
	if txn.Local {
		t.invalidatePosCacheFrom(index)
	}
	counted := 0
	item := t.start
	for item != nil && length > 0 {
		if item.Deleted || !item.Content.IsCountable() {
			item = item.Right
			continue
		}
		n := item.Content.Len()
		if counted+n <= index {
			counted += n
			item = item.Right
			continue
		}
		if counted < index {
			// index falls inside this item; split at the start of the deletion.
			splitAt := index - counted
			right := splitItem(txn, item, splitAt)
			counted = index
			item = right
			n = right.Content.Len()
		}
		if n <= length {
			item.delete(txn)
			length -= n
			item = item.Right
		} else {
			// item extends past the end of the deletion range; split it first.
			splitItem(txn, item, length)
			item.delete(txn)
			length = 0
		}
	}
}
