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
		if item.Deleted || !item.Content.IsCountable() {
			continue
		}
		n := item.Content.Len()
		if counted+n > index {
			if ca, ok := item.Content.(*ContentAny); ok {
				return ca.Vals[index-counted]
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
		if item.Deleted || !item.Content.IsCountable() {
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
	result := make([]any, 0, end-start)
	counted := 0
	for item := t.start; item != nil && counted < end; item = item.Right {
		if item.Deleted || !item.Content.IsCountable() {
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
		if item.Deleted || !item.Content.IsCountable() {
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

// Move removes the element at fromIndex and reinserts it at toIndex.
// Both indices are in terms of the logical (non-deleted) position.
// Note: this is a simplified delete-then-insert implementation; the moved
// element receives a new Item ID and loses its original causal history.
//
// Move walks the linked list directly instead of calling Get() because Get()
// acquires doc.mu.RLock() while Move is called from inside a Transact callback
// that already holds doc.mu.Lock() — acquiring RLock on top of Lock deadlocks.
func (a *YArray) Move(txn *Transaction, fromIndex, toIndex int) {
	if fromIndex == toIndex {
		return
	}
	// Find the element at fromIndex by walking the list (no lock acquisition).
	t := &a.abstractType
	counted := 0
	var elem any
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted || !item.Content.IsCountable() {
			continue
		}
		n := item.Content.Len()
		if counted+n > fromIndex {
			if ca, ok := item.Content.(*ContentAny); ok {
				elem = ca.Vals[fromIndex-counted]
			}
			break
		}
		counted += n
	}
	a.Delete(txn, fromIndex, 1)
	// toIndex is the desired final position of the element in the result array.
	// No index adjustment is required: after deleting fromIndex the caller's
	// requested final position directly maps to the insertion offset in the
	// modified array (the previous adjustment `toIndex--` was incorrect and
	// caused adjacent forward-move to be a no-op).
	a.Insert(txn, toIndex, []any{elem})
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
