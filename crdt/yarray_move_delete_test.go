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
