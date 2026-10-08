package crdt

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func indexOf(arr *YArray, v any) int {
	for i, x := range arr.ToSlice() {
		if x == v {
			return i
		}
	}
	return -1
}

// Undoing the delete of a re-moved element restores every move, so the
// following undos step back through each one as if it had never been deleted.
func TestUnit_UndoManager_UndoDeleteOfReMovedElement_StepsBackEachMove(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	var updates [][]byte
	doc.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c", "d"}) })
	um := NewUndoManager(doc, []SharedType{arr})
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 2) })
	um.StopCapturing()
	i := indexOf(arr, "a")
	doc.Transact(func(txn *Transaction) { arr.Move(txn, i, 4) })
	um.StopCapturing()
	requireArrayConsistent(t, arr, []any{"b", "c", "d", "a"})
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 3, 1) })
	um.StopCapturing()

	steps := []struct {
		undo bool
		want []any
	}{
		{true, []any{"b", "c", "d", "a"}},
		{true, []any{"b", "c", "a", "d"}},
		{true, []any{"a", "b", "c", "d"}},
		{false, []any{"b", "c", "a", "d"}},
		{false, []any{"b", "c", "d", "a"}},
		{false, []any{"b", "c", "d"}},
		{true, []any{"b", "c", "d", "a"}},
		{true, []any{"b", "c", "a", "d"}},
	}
	for i, s := range steps {
		if s.undo {
			require.True(t, um.Undo(), "step %d", i)
		} else {
			require.True(t, um.Redo(), "step %d", i)
		}
		requireArrayConsistent(t, arr, s.want)
		fresh := newTestDoc(9)
		syncTo(t, doc, fresh)
		requireArrayConsistent(t, fresh.GetArray("a"), s.want)
	}

	want := steps[len(steps)-1].want
	for seed := uint64(0); seed < 8; seed++ {
		order := rand.New(rand.NewPCG(seed, 1)).Perm(len(updates))
		peer := newTestDoc(7)
		for _, i := range order {
			require.NoError(t, ApplyUpdateV1(peer, updates[i], nil))
		}
		requireArrayConsistent(t, peer.GetArray("a"), want)
	}
}

// A remote move outranking the local one: the copy lands at the remote
// destination, and undoing the losing local move leaves it there.
func TestInteg_UndoManager_UndoDeleteOfElementWithRemoteWinningMove(t *testing.T) {
	docA, docB := newTestDoc(2), newTestDoc(1)
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"a", "b", "c", "d"}) })
	syncTo(t, docA, docB)
	um := NewUndoManager(docA, []SharedType{arrA})
	docA.Transact(func(txn *Transaction) { arrA.Move(txn, 0, 4) })
	um.StopCapturing()
	docB.Transact(func(txn *Transaction) { arrB.Move(txn, 0, 2) })
	syncTo(t, docB, docA)
	requireArrayConsistent(t, arrA, []any{"b", "c", "a", "d"})
	docA.Transact(func(txn *Transaction) { arrA.Delete(txn, 2, 1) })
	um.StopCapturing()

	require.True(t, um.Undo())
	requireArrayConsistent(t, arrA, []any{"b", "c", "a", "d"})
	require.True(t, um.Undo())
	requireArrayConsistent(t, arrA, []any{"b", "c", "a", "d"})
	syncTo(t, docA, docB)
	requireArrayConsistent(t, arrB, []any{"b", "c", "a", "d"})
}

// The restored copy's moves belong to the restorer, like any restored
// content (Yjs): the original move is retired, so its author's undo skips it
// as a no-op, whether it runs before or after the restore arrives. Peers
// converge either way.
func TestInteg_UndoManager_RemoteUndoOfMoveAroundRestore_Converges(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		docA, docB := newTestDoc(1), newTestDoc(2)
		arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
		docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"a", "b", "c"}) })
		syncTo(t, docA, docB)
		umB := NewUndoManager(docB, []SharedType{arrB})
		docB.Transact(func(txn *Transaction) { arrB.Push(txn, []any{"x"}) })
		umB.StopCapturing()
		docB.Transact(func(txn *Transaction) { arrB.Move(txn, 0, 2) })
		syncTo(t, docB, docA)
		requireArrayConsistent(t, arrA, []any{"b", "c", "a", "x"})
		um := NewUndoManager(docA, []SharedType{arrA})
		docA.Transact(func(txn *Transaction) { arrA.Delete(txn, 2, 1) })
		require.True(t, um.Undo())
		requireArrayConsistent(t, arrA, []any{"b", "c", "a", "x"})

		want := []any{"b", "c", "a"}
		if concurrent {
			require.True(t, umB.Undo())
			requireArrayConsistent(t, arrB, []any{"a", "b", "c", "x"})
			want = []any{"b", "c", "a", "x"}
		} else {
			syncTo(t, docA, docB)
			require.True(t, umB.Undo(), "skips the retired move, undoes the push")
		}
		syncTo(t, docB, docA)
		syncTo(t, docA, docB)
		requireArrayConsistent(t, arrA, want)
		requireArrayConsistent(t, arrB, want)

		require.True(t, um.Redo())
		require.True(t, um.Undo())
		syncTo(t, docA, docB)
		requireArrayConsistent(t, arrA, want)
		requireArrayConsistent(t, arrB, want)
	}
}

// The moves replaced by the copy's are tombstoned with their redone link, so
// a redone link never sits on a live move.
func TestUnit_UndoManager_UndoDeleteOfMovedElement_RetiresOriginalMoves(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	um := NewUndoManager(doc, []SharedType{arr})
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 2) })
	i := indexOf(arr, "a")
	doc.Transact(func(txn *Transaction) { arr.Move(txn, i, 3) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 2, 1) })
	require.True(t, um.Undo())
	requireArrayConsistent(t, arr, []any{"b", "c", "a"})

	for it := arr.start; it != nil; it = it.Right {
		if it.redone != nil {
			require.True(t, it.Deleted, "%v carries a redone link while live", it.ID)
		}
	}
	require.Equal(t, 2, countLiveMoves(&arr.abstractType))
}

// A restored move deleted again with its container restores against the
// container's copy; undoing the move afterwards still steps the element back.
func TestUnit_UndoManager_UndoDeleteOfMovedElement_ThenContainerDelete(t *testing.T) {
	doc := newTestDoc(1)
	root := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		inner := NewArrayPrelim()
		inner.Push(txn, []any{"a", "b", "c"})
		root.Set(txn, "list", inner)
	})
	um := NewUndoManager(doc, []SharedType{root})
	list := func() *YArray { v, _ := root.Get("list"); return v.(*YArray) }
	inner := list()
	doc.Transact(func(txn *Transaction) { inner.Move(txn, 0, 3) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { inner.Delete(txn, 2, 1) })
	um.StopCapturing()
	require.True(t, um.Undo())
	requireArrayConsistent(t, list(), []any{"b", "c", "a"})
	um.Clear()

	doc.Transact(func(txn *Transaction) { inner.Move(txn, 0, 3) })
	um.StopCapturing()
	requireArrayConsistent(t, list(), []any{"c", "a", "b"})
	doc.Transact(func(txn *Transaction) { root.Delete(txn, "list") })
	um.StopCapturing()
	require.True(t, um.Undo())
	requireArrayConsistent(t, list(), []any{"c", "a", "b"})
	require.True(t, um.Undo())
	requireArrayConsistent(t, list(), []any{"b", "c", "a"})

	fresh := newTestDoc(9)
	syncTo(t, doc, fresh)
	v, _ := fresh.GetMap("m").Get("list")
	requireArrayConsistent(t, v.(*YArray), []any{"b", "c", "a"})
}
