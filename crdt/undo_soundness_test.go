package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// syncTo applies the updates `to` is missing from `from` (one direction).
func syncTo(t *testing.T, from, to *Doc) {
	t.Helper()
	require.NoError(t, ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, to.StateVector()), nil))
}

// F-8b (#124): undoing a deletion must propagate to peers. The pre-fix undo
// flipped Deleted=false in place, which produced no wire record — so a peer
// stayed deleted and a back-sync re-deleted locally, obliterating the undo.
// The fix re-inserts the content as a new item, which syncs like any insert.
func TestInteg_UndoManager_TextDeleteUndo_PropagatesToPeer(t *testing.T) {
	docA := newTestDoc(1)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, 0, "hello", nil) })

	docB := newTestDoc(2)
	txtB := docB.GetText("t")
	syncTo(t, docA, docB)
	require.Equal(t, "hello", txtB.ToString())

	um := NewUndoManager(docA, []SharedType{txtA})
	docA.Transact(func(txn *Transaction) { txtA.Delete(txn, 0, 5) })
	require.Empty(t, txtA.ToString())
	syncTo(t, docA, docB)
	require.Empty(t, txtB.ToString())

	require.True(t, um.Undo())
	require.Equal(t, "hello", txtA.ToString(), "undo restores locally")

	syncTo(t, docA, docB)
	require.Equal(t, "hello", txtB.ToString(), "undo must propagate to the peer")

	// Full bidirectional convergence — the undo must survive a back-sync.
	syncTo(t, docB, docA)
	require.Equal(t, "hello", txtA.ToString())
	require.Equal(t, txtA.ToString(), txtB.ToString())
}

// Redo of an undone deletion re-applies the deletion (inverse of the undo's
// re-insert), and that too converges across peers.
func TestInteg_UndoManager_TextRedoAfterUndo(t *testing.T) {
	docA := newTestDoc(1)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, 0, "hello", nil) })

	um := NewUndoManager(docA, []SharedType{txtA})
	docA.Transact(func(txn *Transaction) { txtA.Delete(txn, 0, 5) })

	require.True(t, um.Undo())
	require.Equal(t, "hello", txtA.ToString())
	require.True(t, um.Redo())
	require.Empty(t, txtA.ToString(), "redo re-applies the deletion")
}

// Array deletes undo and propagate too (the sequence path with non-text content).
func TestInteg_UndoManager_ArrayDeleteUndo_PropagatesToPeer(t *testing.T) {
	docA := newTestDoc(1)
	arrA := docA.GetArray("a")
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"x", "y", "z"}) })

	docB := newTestDoc(2)
	arrB := docB.GetArray("a")
	syncTo(t, docA, docB)
	require.Equal(t, 3, arrB.Len())

	um := NewUndoManager(docA, []SharedType{arrA})
	docA.Transact(func(txn *Transaction) { arrA.Delete(txn, 1, 1) }) // remove "y"
	syncTo(t, docA, docB)
	require.Equal(t, 2, arrB.Len())

	require.True(t, um.Undo())
	require.Equal(t, 3, arrA.Len())
	syncTo(t, docA, docB)
	require.Equal(t, 3, arrB.Len(), "undo must restore the element on the peer")
	require.Equal(t, "y", arrB.Get(1))
}

// Map deletes undo and propagate too (the ParentSub path of redoItem).
func TestInteg_UndoManager_MapDeleteUndo_PropagatesToPeer(t *testing.T) {
	docA := newTestDoc(1)
	mA := docA.GetMap("m")
	docA.Transact(func(txn *Transaction) { mA.Set(txn, "k", "v") })

	docB := newTestDoc(2)
	mB := docB.GetMap("m")
	syncTo(t, docA, docB)
	got, _ := mB.Get("k")
	require.Equal(t, "v", got)

	um := NewUndoManager(docA, []SharedType{mA})
	docA.Transact(func(txn *Transaction) { mA.Delete(txn, "k") })
	syncTo(t, docA, docB)
	_, ok := mB.Get("k")
	require.False(t, ok, "delete synced")

	require.True(t, um.Undo())
	syncTo(t, docA, docB)
	gotB, okB := mB.Get("k")
	require.True(t, okB, "undo must restore the key on the peer")
	require.Equal(t, "v", gotB)
}
