package crdt

import (
	"encoding/json"
)

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
	// prelim stages the CONTENT of this array while it is detached, mirroring
	// Yjs's _prelimContent: mutations edit the staged slice directly and the
	// net result materialises once, when the container item integrates.
	// Materialising eagerly would give children clocks BELOW the future
	// container item's — an ordering genuine Yjs never produces. Entries are
	// plain values or detached shared types.
	prelim []any
}

// flushPrelim materialises the staged content when the container item
// integrates. Runs of consecutive plain values become a single item and each
// nested type gets its own, which is what Yjs's typeListInsertGenerics does
// with _prelimContent — so a detached build emits the same structs Yjs emits
// rather than one per call.
func (a *YArray) flushPrelim(txn *Transaction) {
	staged := a.prelim
	a.prelim = nil

	var run []any
	flushRun := func() {
		if len(run) > 0 {
			a.Push(txn, run)
			run = nil
		}
	}
	for _, v := range staged {
		if st, ok := v.(sharedType); ok {
			flushRun()
			a.PushType(txn, st)
			continue
		}
		run = append(run, v)
	}
	flushRun()
}

// prelimValueAt unwraps a staged entry for reading: a staged shared type is
// surfaced as its owner, the same handle an attached read would return.
func prelimValueAt(v any) any {
	if st, ok := v.(sharedType); ok {
		return st.baseType().owner
	}
	return v
}

// prelimJSONValue renders a staged entry the way toJSONValue renders an
// attached one, so a detached ToJSON/ToSlice/Entries unwraps nested staged
// types recursively rather than emitting an opaque handle.
func prelimJSONValue(v any) any {
	switch owner := v.(type) {
	case *YArray:
		return owner.toSliceLocked()
	case *YMap:
		return owner.entriesLocked()
	case *YText:
		return owner.toStringLocked()
	case *YXmlElement:
		return owner.toXMLLocked()
	case *YXmlText:
		return owner.toXMLLocked()
	// No YXmlFragment case: a fragment is only ever a named root or a decode
	// product, so a detached one cannot be obtained to stage in the first place.
	default:
		return v
	}
}

func (a *YArray) baseType() *abstractType { return &a.abstractType }

// prepareFire snapshots the current observer slice inside the document write
// lock and returns a closure that fires all snapshotted observers. Callers in
// Transact invoke the returned closure after releasing the lock, so observers
// may safely call back into any Doc method (N-C1).
//
// prepareFire is called by buildPhase2 while the document write lock is held.
//
// The Delta is computed under the lock so it sees a consistent view of the
// linked list before the lock is released. Added in v1.15.0 (#74 D1).
func (a *YArray) prepareFire(txn *Transaction, _ map[string]struct{}) func() {
	if len(a.observers) == 0 {
		return nil
	}
	delta := a.computeDelta(txn)
	snap := make([]arraySub, len(a.observers))
	copy(snap, a.observers)
	e := YArrayEvent{Target: a, Txn: txn, Delta: delta}
	return func() {
		for _, s := range snap {
			s.fn(e)
		}
	}
}

// computeDelta builds a Quill-compatible delta for the array changes in
// txn — mirrors YText.computeDelta but for array semantics. Walks items in
// linked-list order; for each item, classifies as:
//   - new + not deleted   → Insert with the values
//   - new + deleted       → no-op (transient)
//   - pre-existing, now-deleted → Delete N
//   - pre-existing, still-live  → Retain N (consecutive retains coalesce
//     into a single op so the emitted delta is compact)
//
// Move semantics mirror the render walk used by Get / ToSlice:
//   - a winning ContentMove (MovedBy of the target points back at this item)
//     renders as the target's values at this position; for a new winning
//     move that means an Insert; for a pre-existing winning move the
//     destination has not changed, so Retain N.
//   - items with MovedBy != nil are rendered elsewhere and therefore must
//     not appear at their original position; when the move-away happened
//     this transaction the original position emits a Delete, otherwise the
//     item is silently skipped (already invisible before the txn).
//
// Trailing Retain is elided per Quill convention.
func (a *YArray) computeDelta(txn *Transaction) []Delta {
	var ops []Delta
	retain := 0
	flushRetain := func() {
		if retain > 0 {
			ops = append(ops, Delta{Op: DeltaOpRetain, Retain: retain})
			retain = 0
		}
	}

	t := &a.abstractType
	for item := t.start; item != nil; item = item.Right {
		if item.ParentSub != nil {
			// Map-keyed entries don't belong to the array's sequence; skip.
			continue
		}

		// Move-aware classification — see contract above.
		if cm, ok := item.Content.(*ContentMove); ok {
			// Resolve the target this ContentMove claims. Render only if
			// this move is the current winner for the target.
			if a.doc == nil || cm.Target == nil {
				continue
			}
			target := a.doc.store.Find(*cm.Target)
			if target == nil || target.MovedBy != item || target.Deleted || item.Deleted {
				continue
			}
			n := target.Content.Len()
			beforeClock := txn.beforeState.Clock(item.ID.Client)
			isNew := item.ID.Clock >= beforeClock
			if isNew {
				flushRetain()
				ops = append(ops, Delta{
					Op:     DeltaOpInsert,
					Insert: arrayValuesFromItem(target),
				})
			} else {
				retain += n
			}
			continue
		}
		if item.MovedBy != nil {
			// Item is rendered at the ContentMove's position, not here. If
			// the move-away happened this transaction the original position
			// emits a Delete; otherwise the item was already invisible.
			if !item.Content.IsCountable() {
				continue
			}
			beforeClock := txn.beforeState.Clock(item.MovedBy.ID.Client)
			moveIsNew := item.MovedBy.ID.Clock >= beforeClock
			if moveIsNew && !item.Deleted {
				flushRetain()
				ops = append(ops, Delta{Op: DeltaOpDelete, Delete: item.Content.Len()})
			}
			continue
		}

		if !item.Content.IsCountable() {
			continue
		}
		beforeClock := txn.beforeState.Clock(item.ID.Client)
		isNew := item.ID.Clock >= beforeClock
		n := item.Content.Len()

		if isNew {
			if !item.Deleted {
				flushRetain()
				ops = append(ops, Delta{
					Op:     DeltaOpInsert,
					Insert: arrayValuesFromItem(item),
				})
			}
			// new + deleted → transient; skip
		} else if txn.deleteSet.IsDeleted(item.ID) {
			flushRetain()
			ops = append(ops, Delta{Op: DeltaOpDelete, Delete: n})
		} else if !item.Deleted {
			retain += n
		}
	}
	// Trailing retain is elided.
	return ops
}

// arrayValuesFromItem extracts the slice of values an array item contributes
// to a Delta's Insert. Returns []any in all cases, mirroring Yjs JS where
// `event.delta` entries always carry an array of values.
func arrayValuesFromItem(item *Item) []any {
	switch c := item.Content.(type) {
	case *ContentAny:
		out := make([]any, len(c.Vals))
		copy(out, c.Vals)
		return out
	case *ContentJSON:
		out := make([]any, len(c.Vals))
		copy(out, c.Vals)
		return out
	case *ContentEmbed:
		return []any{c.Val}
	case *ContentType:
		return []any{toJSONValue(c)}
	}
	return nil
}

// Len returns the number of non-deleted elements.
func (a *YArray) Len() int {
	if a.detached() {
		return len(a.prelim)
	}
	return a.length
}

// spliceInto inserts vals at index, clamping index into range. The three-index
// slice on the head keeps the append from writing into the tail's backing
// array.
func spliceInto(dst []any, index int, vals []any) []any {
	if index < 0 {
		index = 0
	}
	if index > len(dst) {
		index = len(dst)
	}
	out := make([]any, 0, len(dst)+len(vals))
	out = append(out, dst[:index]...)
	out = append(out, vals...)
	return append(out, dst[index:]...)
}

// rejectSharedVals guards the plain-value entry points. A shared type batched
// into a ContentAny only fails at encode time — inside Doc.Transact when an
// OnUpdate hook triggers commit-time encoding — so fail at the call site
// instead. Runs before the detached gate so a staged value cannot defer the
// panic to attach time.
func rejectSharedVals(vals []any) {
	for _, v := range vals {
		if _, ok := v.(sharedType); ok {
			panic("crdt: a shared type cannot be inserted as a plain value; use PushType")
		}
	}
}

// Insert inserts vals at logical position index (0 = prepend, Len() = append).
func (a *YArray) Insert(txn *Transaction, index int, vals []any) {
	rejectSharedVals(vals)
	if a.detached() {
		a.prelim = spliceInto(a.prelim, index, vals)
		return
	}
	t := &a.abstractType
	left, offset := t.leftNeighbourAt(index)
	if offset > 0 {
		splitItem(txn, left, offset)
		// left is now the left half; its Right points to the right half.
	}
	a.insertAfterItem(txn, left, vals, index)
}

// insertAfterItem integrates a new item holding vals immediately after left
// (left == nil means "at the head"). hintIndex is the logical position of the
// insertion, used only for partial pos-cache invalidation. Shared by Insert
// and Push; the two differ only in how left is chosen (Insert uses the
// live-index neighbour, Push uses the physical tail).
func (a *YArray) insertAfterItem(txn *Transaction, left *Item, vals []any, hintIndex int) {
	t := &a.abstractType

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
	if hintIndex > 0 {
		t.insertHint = hintIndex
	}
	item.integrate(txn, 0)
}

// Push appends vals to the end of the array, matching Yjs's push
// (typeListPushGenerics): the new element anchors after the last PHYSICAL item,
// tombstones included — NOT after the last live element. Insert(Len()) would
// use live-index semantics (leftNeighbourAt skips tombstones), so when the tail
// is a tombstone it anchors the new item before it (origin=nil,
// rightOrigin=tombstone) whereas Yjs anchors after it (origin=tombstone,
// rightOrigin=nil). Those are different YATA anchors, so a concurrent push from
// a Yjs peer would order the two results differently — a convergence divergence
// surfaced by the #70 cross-impl fuzz oracle.
func (a *YArray) Push(txn *Transaction, vals []any) {
	rejectSharedVals(vals)
	if a.detached() {
		a.prelim = append(a.prelim, vals...)
		return
	}
	t := &a.abstractType
	// Start from the last live item (fast, pos-cached) then walk past any
	// trailing tombstones to the physical tail. When there are no trailing
	// tombstones this is identical to Insert(Len()) and just as cheap.
	last, _ := t.leftNeighbourAt(t.length)
	if last == nil {
		last = t.start // all items deleted (leading tombstone), or nil if empty
	}
	for last != nil && last.Right != nil {
		last = last.Right
	}
	a.insertAfterItem(txn, last, vals, t.length)
}

// Get returns the element at logical position index, or nil if out of bounds.
// Must not be called from inside a Transact callback — acquires a read lock
// that would deadlock with the write lock held by Transact.
func (a *YArray) Get(index int) any {
	if doc := a.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	if a.detached() {
		if index < 0 || index >= len(a.prelim) {
			return nil
		}
		return prelimValueAt(a.prelim[index])
	}
	t := &a.abstractType
	if index < 0 {
		return nil
	}

	// Single position definition (G5): findMarkerRO brackets the PHYSICAL item
	// whose rendered range [start, start+n) contains index, using renderedStep
	// — the one shared, move-aware notion of how each item contributes to
	// rendered position. It internally handles the force-cold seam
	// (t.disableMarkers → full walk from t.start) and an empty marker cache, so
	// there is no separate inline cold walk to drift out of sync. It is safe
	// under RLock (never mutates t.markers).
	//
	// index+1 converts Get's "which element" (0-based) numbering to
	// findMarkerRO's "insert-before" numbering: findMarkerRO(index+1) brackets
	// the item whose rendered range contains index, with start == the item's
	// own rendered start. The tie-break at an exact item boundary agrees too:
	// findMarkerRO returns the earlier item when start+n == index+1.
	//
	// For a winning ContentMove the bracketing physical item is the ContentMove
	// itself; renderedStep reports its rendered contribution as the moved
	// target's, so renderAt points at the value-bearing target item. For a
	// plain item renderAt is nil and the value comes from the item itself.
	item, start := t.findMarkerRO(index + 1)
	if item == nil {
		return nil
	}
	valItem := item
	if _, _, renderAt := t.renderedStep(item); renderAt != nil {
		valItem = renderAt
	}
	switch c := valItem.Content.(type) {
	case *ContentAny:
		return c.Vals[index-start]
	case *ContentType:
		return c.Type.owner
	}
	return nil
}

// Delete removes length elements starting at logical position index.
func (a *YArray) Delete(txn *Transaction, index, length int) {
	if a.detached() {
		// Staged content is removed outright, so nothing is emitted for it and
		// no tombstone reaches the wire — matching a detached Yjs delete.
		if index < 0 || length <= 0 || index >= len(a.prelim) {
			return
		}
		if index+length > len(a.prelim) {
			length = len(a.prelim) - index
		}
		a.prelim = append(a.prelim[:index], a.prelim[index+length:]...)
		return
	}
	deleteRange(&a.abstractType, txn, index, length)
}

// ToSlice returns all non-deleted elements as a new slice. Nested shared
// types are recursively unwrapped via toJSONValue (#75): a nested YArray
// appears as []any, a nested YMap as map[string]any, a nested YText as
// string. Pre-fix these were silently dropped from the output.
//
// Must not be called from inside a Transact callback.
func (a *YArray) ToSlice() []any {
	if doc := a.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	return a.toSliceLocked()
}

// toSliceLocked is the lock-free body of ToSlice; callers must already
// hold the doc lock. Used by ToSlice (top-level) and toJSONValue (during
// recursive unwrap of nested types under #75).
func (a *YArray) toSliceLocked() []any {
	if a.detached() {
		out := make([]any, 0, len(a.prelim))
		for _, v := range a.prelim {
			out = append(out, prelimJSONValue(v))
		}
		return out
	}
	t := &a.abstractType
	result := make([]any, 0, t.length)
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		if cm, ok := item.Content.(*ContentMove); ok {
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
			continue
		}
		switch c := item.Content.(type) {
		case *ContentAny:
			result = append(result, c.Vals...)
		case *ContentJSON:
			// ContentJSON is the legacy JSON wire variant (tag wireJSON=2),
			// functionally equivalent to ContentAny. Updates received from
			// JS peers can land as ContentJSON items; without this case they
			// would be silently dropped from ToSlice/ToJSON output.
			result = append(result, c.Vals...)
		case *ContentEmbed:
			result = append(result, c.Val)
		case *ContentType:
			result = append(result, toJSONValue(c))
		}
	}
	return result
}

// toJSONValue recursively unwraps a ContentType into its JSON-shaped value.
// YArray → []any, YMap → map[string]any, YText → string, YXmlElement /
// YXmlFragment / YXmlText → string (XML serialisation). Unknown nested
// types fall back to nil. Caller must hold the doc lock. See #75.
func toJSONValue(ct *ContentType) any {
	if ct == nil || ct.Type == nil || ct.Type.owner == nil {
		return nil
	}
	switch owner := ct.Type.owner.(type) {
	case *YArray:
		return owner.toSliceLocked()
	case *YMap:
		return owner.entriesLocked()
	case *YText:
		return owner.toStringLocked()
	case *YXmlElement:
		return owner.toXMLLocked()
	case *YXmlFragment:
		return owner.toXMLLocked()
	case *YXmlText:
		return owner.toXMLLocked()
	default:
		return nil
	}
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

	// Marker fast path: jump straight to the physical item bracketing `start`
	// via findMarkerRO instead of walking from t.start (it handles the
	// force-cold seam and empty cache internally, so the disableMarkers path
	// funnels through the same code). Once positioned, the loop still visits
	// every item through `end` to collect values, so only the O(start) prefix
	// walk is skipped, not the O(end-start) work.
	//
	// The loop derives countability, rendered length and the value-bearing
	// item from renderedStep — the same shared, move-aware definition
	// findMarkerRO used to position `counted` — so the collection walk can
	// never drift out of step with the bracket it started from.
	item, counted := t.findMarkerRO(start + 1)
	for ; item != nil && counted < end; item = item.Right {
		countable, n, renderAt := t.renderedStep(item)
		if !countable {
			continue
		}
		valItem := item
		if renderAt != nil {
			valItem = renderAt
		}
		ca, ok := valItem.Content.(*ContentAny)
		if !ok {
			// Countable but not a plain-value item (e.g. a nested ContentType):
			// advance the rendered cursor by its full contribution without
			// emitting values, matching the position accounting used to bracket
			// `start`.
			counted += n
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
	if a.detached() {
		// Nested types are skipped and do not advance the index, which is what
		// the attached walk below does — it yields ContentAny values only.
		index := 0
		for _, v := range a.prelim {
			if _, isType := v.(sharedType); isType {
				continue
			}
			fn(index, v)
			index++
		}
		return
	}
	t := &a.abstractType
	index := 0
	// Same shared position definition as Get/Slice: renderedStep decides
	// countability and, for a winning ContentMove, points renderAt at the
	// value-bearing target so the move is expanded at its destination.
	for item := t.start; item != nil; item = item.Right {
		countable, _, renderAt := t.renderedStep(item)
		if !countable {
			continue
		}
		valItem := item
		if renderAt != nil {
			valItem = renderAt
		}
		if ca, ok := valItem.Content.(*ContentAny); ok {
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
	if a.detached() {
		// Reorder the staged slice, so a detached move emits ordinary content
		// rather than a ContentMove other implementations cannot decode.
		if fromIndex < 0 || fromIndex >= len(a.prelim) || toIndex < 0 || toIndex > len(a.prelim) || fromIndex == toIndex {
			return
		}
		v := a.prelim[fromIndex]
		rest := append(a.prelim[:fromIndex:fromIndex], a.prelim[fromIndex+1:]...)
		if toIndex > fromIndex {
			toIndex--
		}
		a.prelim = spliceInto(rest, toIndex, []any{v})
		return
	}
	if fromIndex == toIndex {
		return
	}
	t := &a.abstractType

	// Walk the rendered array to find the item at fromIndex, using the same
	// shared renderedStep definition as Get/Slice/ForEach and the marker walk:
	// a winning ContentMove is expanded at its destination (renderAt = the
	// moved target), and an item that moved away contributes nothing at its
	// origin. targetItem is the value-bearing item to relocate.
	counted := 0
	var targetItem *Item
	var targetOff int
	for item := t.start; item != nil; item = item.Right {
		countable, n, renderAt := t.renderedStep(item)
		if !countable {
			continue
		}
		if counted+n > fromIndex {
			targetItem = item
			if renderAt != nil {
				targetItem = renderAt
			}
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
	// Search-marker maintenance (G1) happens AFTER the tombstoning below: once
	// the affected items carry Deleted=true, updateMarkerChanges(index, -deleted)
	// both drops markers that now point at a tombstone and shifts the survivors
	// after the deleted region left by the number of rendered positions removed,
	// preserving markers before index for a subsequent nearby lookup. Doing it
	// up-front (the old drop-from-index shim) would discard reusable markers and
	// could not distinguish shift-vs-drop. splitItem, if a boundary split is
	// needed during the walk, clears all markers, in which case the call below
	// is a no-op on the empty set — still correct.
	origLen := length
	var item *Item
	var counted int
	if index <= 0 {
		// index<=0 has no meaningful "bracket item" under findMarkerMut (it
		// always answers (nil,0) there, by design — that's the correct
		// leftNeighbourAt/insert-before-everything answer, not a delete
		// start). Start the walk at firstLiveFromStart, not t.start: leading
		// tombstones accumulated by earlier head-deletes are skipped in O(1)
		// via the cache, turning the previous O(N) per-call leading-skip into
		// O(1) amortized. firstLiveFromStart returns the first non-deleted
		// item; subsequent non-countable items (e.g. ContentFormat) are still
		// handled by the existing skip branch below. Closes the deleteRange
		// half of #86.
		item = t.firstLiveFromStart()
		counted = 0
	} else {
		// Marker fast path: findMarkerMut brackets index directly (counted <=
		// index <= counted+n) instead of summing lengths from the document
		// head, closing the O(index) walk this function used to do on every
		// call. Its tie-break (returns the earlier item when an item's range
		// ends exactly at index) is reconciled by the same "counted+n <=
		// index: skip forward" branch below that a full walk from t.start
		// would also have to pass through — the very next loop iteration
		// advances past a boundary-tied item exactly as if we'd walked here.
		// It also respects t.disableMarkers internally (falls back to the
		// identical cold walk), so this is safe under the force-cold test
		// seam too. As a write-path call it also installs/refreshes a marker
		// at the pre-delete position; updateMarkerChanges below (which always
		// runs afterward) correctly shifts or drops it along with every other
		// marker once the range is tombstoned.
		item, counted = t.findMarkerMut(index)
	}
	for item != nil && length > 0 {
		// Rendered contribution via the single shared, move-aware definition —
		// the SAME one Get/Slice/ForEach use to resolve values — so deleteRange's
		// counting can never drift out of step with what Get reports at a rendered
		// index (the #181 bug: the old loop counted raw physical IsCountable/Len,
		// so on a moved array it skipped the winning ContentMove, counted the
		// moved-away item at its origin, and thus deleted the wrong element).
		countable, n, renderAt := t.renderedStep(item)
		if !countable {
			item = item.Right
			continue
		}
		if counted+n <= index {
			counted += n
			item = item.Right
			continue
		}
		if renderAt != nil {
			// Winning ContentMove: the rendered values come from the moved TARGET
			// (renderAt), not from this ContentMove item. TargetLen is always 1
			// (Move() forces single-element targets; resolveMovedItem trims remote
			// moves to it), so n == 1 — the moved element is atomic and can only be
			// fully within the deletion range here (counted >= index, since an
			// integer index cannot land strictly inside a width-1 rendered step).
			// Deleting the target makes BOTH the target's origin slot and this
			// ContentMove render nothing (renderedStep drops a deleted-target move),
			// which is exactly how Yjs removes a moved element: the DELETE lands on
			// the moved content, matching Get(i). No split of the target is needed
			// or safe (splitItem would not carry MovedBy to the right half).
			//
			// Defensive guard (#181 follow-up): the line above assumes n <= length,
			// which today always holds because n == 1 (TargetLen is forced to 1 by
			// Move() and resolveMovedItem only ever clamps a remote target DOWN to
			// <= TargetLen, never merges multiple items up to it) and length >= 1
			// here (the loop guard). But TargetLen travels over the wire
			// (update.go/update_v2.go encode it verbatim), so a hand-built or
			// foreign update could carry TargetLen > 1 against an item that is
			// already exactly that wide, producing n > 1. A library must never
			// panic on wire-derived input, so clamp instead of failing loudly: the
			// width-1 (n == 1) path is untouched and byte-identical, and clamping
			// only bounds how much of the caller's remaining delete length this
			// malformed/foreign multi-width moved target can consume.
			if n > length {
				n = length
			}
			renderAt.delete(txn)
			length -= n
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
	// Shift/drop markers for the rendered positions actually removed. length is
	// whatever could not be deleted (deletion ran past the end), so the number
	// of removed positions is origLen-length. Zero → no-op.
	if deleted := origLen - length; deleted > 0 {
		t.updateMarkerChanges(index, -deleted)
	}
}
