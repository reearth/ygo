package crdt

// Move-aware Insert tests (#181, PR #190 review comment 1): on an array with an
// active ContentMove, Insert must anchor at the correct PHYSICAL neighbour for
// the requested RENDERED position, so the inserted element lands where the
// move-aware Get/Slice/deleteRange would place rendered index i.
//
// The subtle path is leftNeighbourAt's append/beyond-end fallback (hit when the
// requested index exceeds the rendered length). It used to walk t.start tracking
// the last `!Deleted && IsCountable` item — which is MOVE-BLIND: a winning
// ContentMove is non-countable (its rendered element would be skipped) and a
// moved-away item is still countable (but renders elsewhere). When the winning
// ContentMove is the PHYSICAL tail (element moved TO the end), the old fallback
// picked the last plain item instead of the ContentMove, so an append-beyond-end
// slotted the new element BEFORE the moved element instead of after it. The fix
// walks via the shared renderedStep so the fallback anchors after the last item
// that actually RENDERS.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildMovedArray builds [a,b,c,d,e], applies Move(from,to), and optionally
// forces the marker-free cold walk (disableMarkers) so every assertion can be
// exercised on both the marker fast path and the cold oracle.
func buildMovedArray(t *testing.T, from, to int, coldWalk bool) (*Doc, *YArray) {
	t.Helper()
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	arr.disableMarkers = coldWalk
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c", "d", "e"}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, from, to) })
	return doc, arr
}

// assertInsertCoherent inserts vals at index and asserts (1) the rendered order
// equals want, and (2) Insert is coherent with the move-aware readers: ToSlice,
// Get(i) for every i, and Slice(0,Len) all agree.
func assertInsertCoherent(t *testing.T, doc *Doc, a *YArray, index int, vals []any, want []any) {
	t.Helper()
	doc.Transact(func(txn *Transaction) { a.Insert(txn, index, vals) })
	got := a.ToSlice()
	assert.Equal(t, want, got, "Insert(%d) rendered order", index)
	assert.Equal(t, got, getAll(a), "Get coherent with ToSlice after Insert(%d)", index)
	assert.Equal(t, got, a.Slice(0, a.Len()), "Slice coherent with ToSlice after Insert(%d)", index)
}

// Element moved TO the end: [a,b,c,d,e] + Move(0->4) renders [b,c,d,e,a]. The
// winning ContentMove is the PHYSICAL tail; the last plain item ("e"...merged
// [b,c,d,e]) is NOT the rendered tail. This is the move-blind fallback's failure
// case: an append-beyond-end must land AFTER the moved "a".
func TestUnit_YArray_InsertMoved_AppendBeyondEnd_AfterMovedTail(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(map[bool]string{false: "markers", true: "cold"}[cold], func(t *testing.T) {
			doc, arr := buildMovedArray(t, 0, 4, cold)
			require.Equal(t, []any{"b", "c", "d", "e", "a"}, arr.ToSlice(), "move renders")
			require.Equal(t, "a", arr.Get(4), "moved element renders at the tail")

			// Insert BEYOND the end (index > Len): must append after the rendered
			// tail "a". Old move-blind fallback anchored after the last plain item
			// and produced [b,c,d,e,z,a] instead.
			assertInsertCoherent(t, doc, arr, arr.Len()+1, []any{"z"},
				[]any{"b", "c", "d", "e", "a", "z"})
		})
	}
}

// Append exactly at Len() (in-bounds, does NOT hit the fallback — goes through
// the move-aware findMarkerMut). Must also land after the moved tail.
func TestUnit_YArray_InsertMoved_AppendAtLen_AfterMovedTail(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(map[bool]string{false: "markers", true: "cold"}[cold], func(t *testing.T) {
			doc, arr := buildMovedArray(t, 0, 4, cold)
			require.Equal(t, []any{"b", "c", "d", "e", "a"}, arr.ToSlice())
			assertInsertCoherent(t, doc, arr, arr.Len(), []any{"z"},
				[]any{"b", "c", "d", "e", "a", "z"})
		})
	}
}

// Mid insert on a moved array: [b,c,d,e,a] + Insert(2,"z") -> [b,c,z,d,e,a].
func TestUnit_YArray_InsertMoved_Mid(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(map[bool]string{false: "markers", true: "cold"}[cold], func(t *testing.T) {
			doc, arr := buildMovedArray(t, 0, 4, cold)
			assertInsertCoherent(t, doc, arr, 2, []any{"z"},
				[]any{"b", "c", "z", "d", "e", "a"})
		})
	}
}

// Element moved TO the FRONT: [a,b,c,d,e] + Move(4->0) renders [e,a,b,c,d]. Here
// the moved-away item ("e") is the physical tail; both fallbacks happen to agree
// (a moved-away tail renders at the front, so appending physically after it still
// renders last) — this pins that behaviour so the fix doesn't regress it.
func TestUnit_YArray_InsertMoved_AppendBeyondEnd_MovedToFront(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(map[bool]string{false: "markers", true: "cold"}[cold], func(t *testing.T) {
			doc, arr := buildMovedArray(t, 4, 0, cold)
			require.Equal(t, []any{"e", "a", "b", "c", "d"}, arr.ToSlice(), "move-to-front renders")
			assertInsertCoherent(t, doc, arr, arr.Len()+1, []any{"z"},
				[]any{"e", "a", "b", "c", "d", "z"})
		})
	}
}

// Insert at the head on a moved array is unaffected by the fallback but must
// still be coherent: [b,c,d,e,a] + Insert(0,"z") -> [z,b,c,d,e,a].
func TestUnit_YArray_InsertMoved_Head(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(map[bool]string{false: "markers", true: "cold"}[cold], func(t *testing.T) {
			doc, arr := buildMovedArray(t, 0, 4, cold)
			assertInsertCoherent(t, doc, arr, 0, []any{"z"},
				[]any{"z", "b", "c", "d", "e", "a"})
		})
	}
}

// Convergence: peer 1 moves an element to the end then appends beyond the end;
// peer 2 receives the updates and must converge to the identical rendered order.
// Guards that the anchor the move-aware fallback picks is a real, wire-encodable
// YATA position (not just locally correct).
func TestInteg_YArray_InsertMoved_AppendBeyondEnd_Converges(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	arr1 := doc1.GetArray("list")
	arr2 := doc2.GetArray("list")

	doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{"a", "b", "c", "d", "e"}) })
	doc1.Transact(func(txn *Transaction) { arr1.Move(txn, 0, 4) })            // -> [b,c,d,e,a]
	doc1.Transact(func(txn *Transaction) { arr1.Insert(txn, 6, []any{"z"}) }) // beyond end -> append
	require.Equal(t, []any{"b", "c", "d", "e", "a", "z"}, arr1.ToSlice())

	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, nil), nil))
	assert.Equal(t, arr1.ToSlice(), arr2.ToSlice(), "peers converge")
	assert.Equal(t, []any{"b", "c", "d", "e", "a", "z"}, arr2.ToSlice())
}
