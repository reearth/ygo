package crdt

// Move-aware deleteRange tests (#181): on an array with an active ContentMove,
// Get/Slice and Delete must agree about which element renders at rendered index
// i. Delete(i, n) must remove exactly the elements Get reports at rendered
// indices [i, i+n); for a winning ContentMove the delete applies to the moved
// TARGET item (the moved content), matching Yjs YArray move+delete semantics.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getAll snapshots the rendered elements via Get(i) so tests can assert Delete
// removed exactly the elements Get reported (the coherence property).
func getAll(a *YArray) []any {
	out := make([]any, 0, a.Len())
	for i := 0; i < a.Len(); i++ {
		out = append(out, a.Get(i))
	}
	return out
}

// assertDeleteRemovesRendered deletes [i, i+n) and asserts the result equals the
// pre-delete rendered slice with exactly indices [i, i+n) removed — i.e. Delete
// removed exactly what Get reported there.
func assertDeleteRemovesRendered(t *testing.T, doc *Doc, a *YArray, i, n int) {
	t.Helper()
	before := getAll(a)
	require.GreaterOrEqual(t, len(before), i+n, "range in bounds")
	want := make([]any, 0, len(before)-n)
	want = append(want, before[:i]...)
	want = append(want, before[i+n:]...)
	doc.Transact(func(txn *Transaction) { a.Delete(txn, i, n) })
	assert.Equal(t, want, a.ToSlice(), "Delete(%d,%d) removes rendered [%d,%d)", i, n, i, i+n)
	// Coherence: Get after delete matches ToSlice.
	assert.Equal(t, a.ToSlice(), getAll(a), "Get coherent with ToSlice after delete")
}

func newMovedArray(t *testing.T, from, to int) (*Doc, *YArray) {
	t.Helper()
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c", "d", "e"}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, from, to) })
	return doc, arr
}

// [a,b,c,d,e] + Move(4->1) renders [a,e,b,c,d]; Get(1)=="e".
func TestUnit_YArray_DeleteMoved_WinningMove_DeletesTarget(t *testing.T) {
	doc, arr := newMovedArray(t, 4, 1)
	require.Equal(t, []any{"a", "e", "b", "c", "d"}, arr.ToSlice(), "move renders")
	require.Equal(t, "e", arr.Get(1), "Get(1) is the moved element")
	assertDeleteRemovesRendered(t, doc, arr, 1, 1) // must delete "e", not "b"
	assert.Equal(t, []any{"a", "b", "c", "d"}, arr.ToSlice())
}

// Delete a plain element AFTER the moved one: rendered [a,e,b,c,d], Delete(2,1)
// removes "b".
func TestUnit_YArray_DeleteMoved_PlainAfterMove(t *testing.T) {
	doc, arr := newMovedArray(t, 4, 1)
	require.Equal(t, "b", arr.Get(2))
	assertDeleteRemovesRendered(t, doc, arr, 2, 1)
	assert.Equal(t, []any{"a", "e", "c", "d"}, arr.ToSlice())
}

// Multi-delete spanning the moved element: [a,e,b,c,d], Delete(1,2) removes
// "e","b".
func TestUnit_YArray_DeleteMoved_MultiSpanningMove(t *testing.T) {
	doc, arr := newMovedArray(t, 4, 1)
	assertDeleteRemovesRendered(t, doc, arr, 1, 2)
	assert.Equal(t, []any{"a", "c", "d"}, arr.ToSlice())
}

// Delete spanning the boundary from a plain element into the moved element:
// [a,e,b,c,d], Delete(0,2) removes "a","e".
func TestUnit_YArray_DeleteMoved_SpanBoundaryIntoMove(t *testing.T) {
	doc, arr := newMovedArray(t, 4, 1)
	assertDeleteRemovesRendered(t, doc, arr, 0, 2)
	assert.Equal(t, []any{"b", "c", "d"}, arr.ToSlice())
}

// Backward move: [a,b,c,d,e] + Move(1->3) renders [a,c,d,b,e]; Delete the moved
// "b" at rendered index 3.
func TestUnit_YArray_DeleteMoved_BackwardMove(t *testing.T) {
	doc, arr := newMovedArray(t, 1, 3)
	require.Equal(t, []any{"a", "c", "d", "b", "e"}, arr.ToSlice(), "backward move renders")
	require.Equal(t, "b", arr.Get(3))
	assertDeleteRemovesRendered(t, doc, arr, 3, 1)
	assert.Equal(t, []any{"a", "c", "d", "e"}, arr.ToSlice())
}

// Deleting every rendered index one at a time keeps Get/Delete coherent and
// drains the array (moved element included) with no ghost elements.
func TestUnit_YArray_DeleteMoved_DrainAllCoherent(t *testing.T) {
	doc, arr := newMovedArray(t, 4, 1) // [a,e,b,c,d]
	for arr.Len() > 0 {
		want := getAll(arr)[1:] // dropping rendered index 0
		doc.Transact(func(txn *Transaction) { arr.Delete(txn, 0, 1) })
		assert.Equal(t, want, arr.ToSlice())
	}
	assert.Equal(t, []any{}, arr.ToSlice())
}

// Head fast path (#86) on a non-moved array must be unaffected.
func TestUnit_YArray_DeleteMoved_HeadFastPathUnmovedIntact(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c", "d"}) })
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 0, 2) })
	assert.Equal(t, []any{"c", "d"}, arr.ToSlice())
}

// TestInteg_YArray_DeleteMovedElement_TwoPeer_ConcurrentNeighborDelete: peer A
// deletes the moved element ("e", rendered via the winning ContentMove) while
// peer B concurrently deletes a different, plain element rendered adjacent to
// it ("b"). The two deletes target two distinct items, so this is an ordinary
// (non-ambiguous) YATA delete-delete case: both removals apply independently
// and both peers converge to the same array with both elements gone,
// regardless of exchange order.
func TestInteg_YArray_DeleteMovedElement_TwoPeer_ConcurrentNeighborDelete(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	arr1 := doc1.GetArray("list")
	arr2 := doc2.GetArray("list")

	doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{"a", "b", "c", "d", "e"}) })
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, nil), nil))

	// Both peers learn the move (created by doc1): [a,e,b,c,d].
	doc1.Transact(func(txn *Transaction) { arr1.Move(txn, 4, 1) })
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, doc2.store.StateVector()), nil))
	require.Equal(t, []any{"a", "e", "b", "c", "d"}, arr1.ToSlice())
	require.Equal(t, []any{"a", "e", "b", "c", "d"}, arr2.ToSlice())

	sv1 := doc1.store.StateVector()
	sv2 := doc2.store.StateVector()

	// Concurrent: doc1 deletes the moved element "e" (rendered index 1);
	// doc2 deletes its rendered neighbor "b" (rendered index 2) — a plain
	// element, not the moved target itself.
	doc1.Transact(func(txn *Transaction) { arr1.Delete(txn, 1, 1) })
	doc2.Transact(func(txn *Transaction) { arr2.Delete(txn, 2, 1) })

	u1to2 := EncodeStateAsUpdateV1(doc1, sv2)
	u2to1 := EncodeStateAsUpdateV1(doc2, sv1)
	require.NoError(t, ApplyUpdateV1(doc2, u1to2, nil))
	require.NoError(t, ApplyUpdateV1(doc1, u2to1, nil))

	s1 := arr1.ToSlice()
	s2 := arr2.ToSlice()
	assert.Equal(t, s1, s2, "peers converge")
	assert.Equal(t, []any{"a", "c", "d"}, s1, "both the moved element and its neighbor are gone")
}

// TestInteg_YArray_DeleteMovedElement_TwoPeer_ConcurrentReMove: peer A deletes
// the moved element ("e") while peer B, still on the pre-delete state,
// concurrently issues a SECOND Move on that same element (re-moving an
// already-moved target — Move() walks through a winning ContentMove to the
// real target item, so this creates a second ContentMove pointing at the same,
// now-concurrently-deleted, target).
//
// This is intentionally not asserted to a single fully-specified physical
// structure: integrate()'s ContentMove arbitration
// (target.MovedBy == nil || item.ID.Client < target.MovedBy.ID.Client) does
// not consult target.Deleted, so which of the two ContentMove items "wins"
// MovedBy is a structural detail. But renderedStep DOES gate on
// target.Deleted before rendering a winning move's target, so a deleted
// target renders nothing no matter which move claims it — the outcome is
// well-defined at the rendered-value level even though the winning-move
// bookkeeping is not something this test pins down. We assert convergence and
// that the deleted element is gone from the rendered result on both peers.
func TestInteg_YArray_DeleteMovedElement_TwoPeer_ConcurrentReMove(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	arr1 := doc1.GetArray("list")
	arr2 := doc2.GetArray("list")

	doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{"a", "b", "c", "d", "e"}) })
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, nil), nil))

	// Both peers learn the first move (created by doc1): [a,e,b,c,d].
	doc1.Transact(func(txn *Transaction) { arr1.Move(txn, 4, 1) })
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, doc2.store.StateVector()), nil))
	require.Equal(t, []any{"a", "e", "b", "c", "d"}, arr1.ToSlice())
	require.Equal(t, []any{"a", "e", "b", "c", "d"}, arr2.ToSlice())

	sv1 := doc1.store.StateVector()
	sv2 := doc2.store.StateVector()

	// Concurrent: doc1 deletes the moved element "e" (rendered index 1);
	// doc2, unaware of the delete, re-moves the SAME element ("e", still at
	// its own rendered index 1) to a new destination.
	doc1.Transact(func(txn *Transaction) { arr1.Delete(txn, 1, 1) })
	doc2.Transact(func(txn *Transaction) { arr2.Move(txn, 1, 3) })

	u1to2 := EncodeStateAsUpdateV1(doc1, sv2)
	u2to1 := EncodeStateAsUpdateV1(doc2, sv1)
	require.NoError(t, ApplyUpdateV1(doc2, u1to2, nil))
	require.NoError(t, ApplyUpdateV1(doc1, u2to1, nil))

	s1 := arr1.ToSlice()
	s2 := arr2.ToSlice()
	assert.Equal(t, s1, s2, "peers converge")
	assert.NotContains(t, s1, "e", "deleted target is gone even though a concurrent re-move also claimed it")
	assert.ElementsMatch(t, []any{"a", "b", "c", "d"}, s1, "the four plain elements survive")
}

// TestUnit_YArray_DeleteMoved_MultiWidthTargetGuardClamps exercises the
// defensive guard in deleteRange's renderAt branch (#181 follow-up). Under
// Move()'s own invariant a ContentMove's target is always width 1, so
// n <= length always holds there. But TargetLen travels over the wire
// (update.go/update_v2.go encode it verbatim) and resolveMovedItem only ever
// clamps a target DOWN to <= TargetLen — it never merges narrower items UP to
// it — so a hand-built or foreign ContentMove with TargetLen > 1 pointing at
// an item that is already that wide (e.g. a single Push of several values)
// can make a winning move render n > 1. This test constructs exactly that
// (bypassing the normal Move() API, which cannot produce it) and asserts
// that deleting fewer rendered positions than the target's width does NOT
// panic — a library must never crash on wire-derived/foreign input — and
// leaves the array in a state where Get/Slice/ToSlice stay coherent with
// each other (no partial, MovedBy-corrupting split of the target; the whole
// target is consumed instead).
func TestUnit_YArray_DeleteMoved_MultiWidthTargetGuardClamps(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	t2 := &arr.abstractType

	var item1 *Item
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"x", "y"}) // single ContentAny item, width 2
		item1 = t2.start
	})
	require.Equal(t, 2, item1.Content.Len(), "single Push of 2 values is one width-2 item")

	doc.Transact(func(txn *Transaction) {
		targetID := item1.ID
		moveItem := &Item{
			ID:          ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Left:        nil,
			OriginRight: &targetID,
			Parent:      t2,
			Content:     NewContentMove(&targetID, 2), // TargetLen=2: violates the width-1 invariant
		}
		moveItem.integrate(txn, 0)
	})
	require.NotNil(t, item1.MovedBy, "hand-built ContentMove must win arbitration")

	// Rendered array is now [x,y] (the moved-in target at index 0). Deleting
	// only 1 rendered position (n=2 > length=1) must NOT panic. The guard
	// clamps n down to the remaining length rather than partially deleting
	// the target (unsafe: splitItem doesn't carry MovedBy to the right
	// half), so the whole width-2 target is consumed by this single-position
	// delete request.
	require.Equal(t, []any{"x", "y"}, arr.ToSlice())
	assert.NotPanics(t, func() {
		doc.Transact(func(txn *Transaction) { arr.Delete(txn, 0, 1) })
	})

	// The clamp fully deletes the moved target (both "x" and "y"); the array
	// is left empty, and Get/Slice/ToSlice/Len must all agree on that — no
	// stale or partially-visible remnant of the malformed move.
	assert.True(t, item1.Deleted, "clamp must fully consume the multi-width target, not partially split it")
	assert.Equal(t, 0, arr.Len())
	assert.Equal(t, []any{}, arr.ToSlice())
	assert.Equal(t, []any{}, getAll(arr))
}

// TestInteg_YArray_DeleteMovedElement_TwoPeer_Converge: peer A deletes a moved
// element while peer B concurrently inserts; after exchanging deltas both peers
// converge and the moved element is gone.
func TestInteg_YArray_DeleteMovedElement_TwoPeer_Converge(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	arr1 := doc1.GetArray("list")
	arr2 := doc2.GetArray("list")

	doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{"a", "b", "c", "d", "e"}) })
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, nil), nil))

	// Both peers learn the move (created by doc1): [a,e,b,c,d].
	doc1.Transact(func(txn *Transaction) { arr1.Move(txn, 4, 1) })
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, doc2.store.StateVector()), nil))
	require.Equal(t, []any{"a", "e", "b", "c", "d"}, arr1.ToSlice())
	require.Equal(t, []any{"a", "e", "b", "c", "d"}, arr2.ToSlice())

	sv1 := doc1.store.StateVector()
	sv2 := doc2.store.StateVector()

	// Concurrent: doc1 deletes the moved element "e" (rendered index 1);
	// doc2 inserts "x" at the head.
	doc1.Transact(func(txn *Transaction) { arr1.Delete(txn, 1, 1) })
	doc2.Transact(func(txn *Transaction) { arr2.Insert(txn, 0, []any{"x"}) })

	u1to2 := EncodeStateAsUpdateV1(doc1, sv2)
	u2to1 := EncodeStateAsUpdateV1(doc2, sv1)
	require.NoError(t, ApplyUpdateV1(doc2, u1to2, nil))
	require.NoError(t, ApplyUpdateV1(doc1, u2to1, nil))

	s1 := arr1.ToSlice()
	s2 := arr2.ToSlice()
	assert.Equal(t, s1, s2, "peers converge")
	assert.NotContains(t, s1, "e", "deleted moved element is gone")
	assert.Contains(t, s1, "x", "concurrent insert survives")
	assert.Equal(t, []any{"x", "a", "b", "c", "d"}, s1)
}
