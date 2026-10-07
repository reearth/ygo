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
