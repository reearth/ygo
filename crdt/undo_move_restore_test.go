package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Undoing the delete of a moved element restores it at the move destination,
// and the move's own undo/redo still applies to the copy.
func TestUnit_UndoManager_UndoDeleteOfMovedElement_RestoresAtDestination(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	um := NewUndoManager(doc, []SharedType{arr})

	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 2) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 2, 1) })
	requireArrayConsistent(t, arr, []any{"b", "c"})

	require.True(t, um.Undo())
	requireArrayConsistent(t, arr, []any{"b", "c", "a"})
	require.True(t, um.Undo())
	requireArrayConsistent(t, arr, []any{"a", "b", "c"})
	require.True(t, um.Redo())
	requireArrayConsistent(t, arr, []any{"b", "c", "a"})
	require.True(t, um.Redo())
	requireArrayConsistent(t, arr, []any{"b", "c"})
	require.True(t, um.Undo())
	requireArrayConsistent(t, arr, []any{"b", "c", "a"})

	fresh := newTestDoc(9)
	syncTo(t, doc, fresh)
	requireArrayConsistent(t, fresh.GetArray("a"), []any{"b", "c", "a"})
}

// A peer's winning move, deleted and restored here: the copy lands at that
// move's destination on every peer, whatever order the updates arrive in.
func TestInteg_UndoManager_UndoDeleteOfRemotelyMovedElement_Converges(t *testing.T) {
	docA, docB := newTestDoc(2), newTestDoc(1)
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"a", "b", "c", "d"}) })
	syncTo(t, docA, docB)
	docB.Transact(func(txn *Transaction) { arrB.Move(txn, 0, 2) })
	syncTo(t, docB, docA)
	requireArrayConsistent(t, arrA, []any{"b", "c", "a", "d"})

	var updates [][]byte
	docA.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	um := NewUndoManager(docA, []SharedType{arrA}, WithTrackedOrigins("local"))
	docA.Transact(func(txn *Transaction) { arrA.Delete(txn, 2, 1) }, "local")
	um.StopCapturing()
	docA.Transact(func(txn *Transaction) { arrA.Insert(txn, 0, []any{"x"}) }, "local")
	require.True(t, um.Undo())
	require.True(t, um.Undo())
	want := []any{"b", "c", "a", "d"}
	requireArrayConsistent(t, arrA, want)

	base := EncodeStateAsUpdateV1(docB, nil)
	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}, {1, 3, 0, 2}} {
		require.Len(t, updates, len(order))
		peer := newTestDoc(7)
		require.NoError(t, ApplyUpdateV1(peer, base, nil))
		for _, i := range order {
			require.NoError(t, ApplyUpdateV1(peer, updates[i], nil))
		}
		requireArrayConsistent(t, peer.GetArray("a"), want)
		reloaded := newTestDoc(8)
		require.NoError(t, ApplyUpdateV2(reloaded, EncodeStateAsUpdateV2(peer, nil), nil))
		requireArrayConsistent(t, reloaded.GetArray("a"), want)
	}
	syncTo(t, docA, docB)
	requireArrayConsistent(t, arrB, want)
}

// A moved element deleted with its unmoved neighbour is restored apart from
// it, so the move still applies.
func TestUnit_UndoManager_UndoDeleteOfMovedNeighbour(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	um := NewUndoManager(doc, []SharedType{arr})
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 1, 3) })
	um.StopCapturing()
	requireArrayConsistent(t, arr, []any{"a", "c", "b"})
	doc.Transact(func(txn *Transaction) {
		arr.Delete(txn, 2, 1)
		arr.Delete(txn, 0, 1)
	})
	requireArrayConsistent(t, arr, []any{"c"})

	require.True(t, um.Undo())
	requireArrayConsistent(t, arr, []any{"a", "c", "b"})
}

// Restoring a deleted container restores a move inside it against the
// restored copy of its target, not the tombstoned original.
func TestUnit_UndoManager_UndoContainerDelete_KeepsInnerMove(t *testing.T) {
	doc := newTestDoc(1)
	root := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		inner := NewArrayPrelim()
		inner.Push(txn, []any{"a", "b", "c"})
		root.Set(txn, "list", inner)
	})
	v, _ := root.Get("list")
	inner := v.(*YArray)
	doc.Transact(func(txn *Transaction) { inner.Move(txn, 0, 2) })
	um := NewUndoManager(doc, []SharedType{root})
	doc.Transact(func(txn *Transaction) { root.Delete(txn, "list") })

	require.True(t, um.Undo())
	v, _ = root.Get("list")
	requireArrayConsistent(t, v.(*YArray), []any{"b", "c", "a"})

	fresh := newTestDoc(9)
	syncTo(t, doc, fresh)
	v, _ = fresh.GetMap("m").Get("list")
	requireArrayConsistent(t, v.(*YArray), []any{"b", "c", "a"})
}

// The container's move is replayed before its target when the target is
// another client's and so restored later; it must restore that target first.
func TestUnit_UndoManager_UndoContainerDelete_KeepsInnerMoveOfRemoteTarget(t *testing.T) {
	doc, remote := newTestDoc(1), newTestDoc(2)
	root := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) { root.Set(txn, "list", NewArrayPrelim()) })
	syncTo(t, doc, remote)
	rv, _ := remote.GetMap("m").Get("list")
	remote.Transact(func(txn *Transaction) { rv.(*YArray).Push(txn, []any{"a", "b", "c"}) })
	syncTo(t, remote, doc)
	v, _ := root.Get("list")
	doc.Transact(func(txn *Transaction) { v.(*YArray).Move(txn, 0, 2) })
	um := NewUndoManager(doc, []SharedType{root})
	doc.Transact(func(txn *Transaction) { root.Delete(txn, "list") })

	require.True(t, um.Undo())
	v, _ = root.Get("list")
	requireArrayConsistent(t, v.(*YArray), []any{"b", "c", "a"})
	require.Equal(t, 1, countLiveMoves(&v.(*YArray).abstractType), "the move is restored once")
	syncTo(t, doc, remote)
	rv, _ = remote.GetMap("m").Get("list")
	requireArrayConsistent(t, rv.(*YArray), []any{"b", "c", "a"})
}

// A moved element inside a run of its neighbours: the run restores around it.
func TestUnit_UndoManager_UndoContainerDelete_KeepsMoveInsideRun(t *testing.T) {
	doc, remote := newTestDoc(1), newTestDoc(2)
	root := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) { root.Set(txn, "list", NewArrayPrelim()) })
	syncTo(t, doc, remote)
	rv, _ := remote.GetMap("m").Get("list")
	remote.Transact(func(txn *Transaction) { rv.(*YArray).Push(txn, []any{"a", "b", "c", "d"}) })
	syncTo(t, remote, doc)
	v, _ := root.Get("list")
	doc.Transact(func(txn *Transaction) { v.(*YArray).Move(txn, 1, 4) })
	um := NewUndoManager(doc, []SharedType{root})
	doc.Transact(func(txn *Transaction) { root.Delete(txn, "list") })

	require.True(t, um.Undo())
	v, _ = root.Get("list")
	requireArrayConsistent(t, v.(*YArray), []any{"a", "c", "d", "b"})
}

// When the move was itself removed meanwhile, the restored element renders
// at its origin, as it would have without the delete.
func TestInteg_UndoManager_UndoDeleteOfMovedElement_MoveUndoneMeanwhile(t *testing.T) {
	docA, docB := newTestDoc(1), newTestDoc(2)
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"a", "b", "c"}) })
	syncTo(t, docA, docB)
	umB := NewUndoManager(docB, []SharedType{arrB})
	docB.Transact(func(txn *Transaction) { arrB.Move(txn, 0, 2) })
	syncTo(t, docB, docA)

	um := NewUndoManager(docA, []SharedType{arrA}, WithTrackedOrigins("local"))
	docA.Transact(func(txn *Transaction) { arrA.Delete(txn, 2, 1) }, "local")
	require.True(t, umB.Undo())
	syncTo(t, docB, docA)
	require.True(t, um.Undo())
	syncTo(t, docA, docB)
	requireArrayConsistent(t, arrA, []any{"a", "b", "c"})
	requireArrayConsistent(t, arrB, []any{"a", "b", "c"})
}

func countLiveMoves(t *abstractType) int {
	n := 0
	for it := t.start; it != nil; it = it.Right {
		if _, ok := it.Content.(*ContentMove); ok && !it.Deleted {
			n++
		}
	}
	return n
}
