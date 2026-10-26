package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// requireArrayConsistent asserts Len, Get(i) and ToSlice agree with want.
func requireArrayConsistent(t *testing.T, arr *YArray, want []any) {
	t.Helper()
	require.Equal(t, want, arr.ToSlice())
	require.Equal(t, len(want), arr.Len())
	for i := range want {
		require.Equal(t, want[i], getAsJSON(arr.Get(i)), "Get(%d)", i)
	}
}

// Undo's direct item.delete bypasses deleteRange's marker shift; a marker past
// the deleted items kept its old index and Get(i) indexed Vals at -1.
func TestUnit_UndoManager_UndoInsert_InvalidatesSearchMarkers(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	um := NewUndoManager(doc, []SharedType{arr})
	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 0, []any{"0", "1", "2"}) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 1, []any{"3"}) })
	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 4, []any{"4"}) })
	requireArrayConsistent(t, arr, []any{"0", "3", "1", "2", "4"})

	require.True(t, um.Undo())
	requireArrayConsistent(t, arr, []any{"0", "1", "2"})
}

// Move sets insertHint for its non-countable ContentMove, which never consumed
// it; the next hintless insert at 0 then shifted markers from the stale hint
// instead of clearing them.
func TestUnit_YArray_MoveDoesNotLeakInsertHint(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 0, []any{"4", "5", "6"}) })
	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 3, []any{"7"}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 2, 3) })
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 2, 1) })
	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 0, []any{"8", "9", "10"}) })
	requireArrayConsistent(t, arr, []any{"8", "9", "10", "4", "5", "6"})
}

// Undoing a Move tombstones its ContentMove; the target must render at its
// origin again rather than vanish while still counted in Len.
func TestUnit_UndoManager_UndoMove_RestoresOrigin(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	um := NewUndoManager(doc, []SharedType{arr})

	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 2) })
	requireArrayConsistent(t, arr, []any{"b", "c", "a"})

	require.True(t, um.Undo())
	requireArrayConsistent(t, arr, []any{"a", "b", "c"})
	require.True(t, um.Redo())
	requireArrayConsistent(t, arr, []any{"b", "c", "a"})
}

// When the winning move is tombstoned, the next live move must take over —
// the same winner a fresh peer computes, which never lets a deleted move claim.
func TestInteg_YArray_DeletedWinningMove_HandsOffToLoser(t *testing.T) {
	docA, docB := newTestDoc(1), newTestDoc(2)
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"a", "b", "c"}) })
	syncTo(t, docA, docB)

	// Track only A's own origin: ApplyUpdate transactions are captured too.
	um := NewUndoManager(docA, []SharedType{arrA}, WithTrackedOrigins("local"))
	docA.Transact(func(txn *Transaction) { arrA.Move(txn, 0, 3) }, "local") // wins: lower client
	docB.Transact(func(txn *Transaction) { arrB.Move(txn, 0, 1) })
	syncTo(t, docA, docB)
	syncTo(t, docB, docA)
	requireArrayConsistent(t, arrA, []any{"b", "c", "a"})
	requireArrayConsistent(t, arrB, []any{"b", "c", "a"})

	require.True(t, um.Undo())
	syncTo(t, docA, docB)
	want := []any{"b", "a", "c"} // B's move now renders
	requireArrayConsistent(t, arrA, want)
	requireArrayConsistent(t, arrB, want)

	fresh := newTestDoc(3)
	syncTo(t, docA, fresh)
	requireArrayConsistent(t, fresh.GetArray("a"), want)
}

// ToSlice rendered a winning move only when its target held plain values, so a
// moved nested type vanished from ToSlice/ToJSON while Get still returned it.
func TestUnit_YArray_ToSlice_RendersMovedNestedType(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"x"})
		m := NewMapPrelim()
		m.Set(txn, "k", "v")
		arr.PushType(txn, m)
	})
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 1, 0) })
	requireArrayConsistent(t, arr, []any{map[string]any{"k": "v"}, "x"})
}
