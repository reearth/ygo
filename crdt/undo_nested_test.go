package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// mapText reads key from m as a *YText's string ("" with ok=false if absent).
func mapText(t *testing.T, m *YMap, key string) string {
	t.Helper()
	v, ok := m.Get(key)
	require.True(t, ok, "key %q missing", key)
	txt, ok := v.(*YText)
	require.True(t, ok, "key %q is %T, want *YText", key, v)
	return txt.ToString()
}

// Undoing the delete of a nested type must restore its children (Yjs redoItem
// re-inserts the children into a fresh copy of the type).
func TestUnit_UndoManager_DeletedNestedText_RestoresChildren(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	um := NewUndoManager(doc, []SharedType{m})

	doc.Transact(func(txn *Transaction) {
		txt := NewTextPrelim()
		txt.Insert(txn, 0, "Hello", nil)
		m.Set(txn, "t", txt)
	})
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "t") })
	require.False(t, m.Has("t"))

	require.True(t, um.Undo())
	require.Equal(t, "Hello", mapText(t, m, "t"))

	require.True(t, um.Redo())
	require.False(t, m.Has("t"))
	require.True(t, um.Undo())
	require.Equal(t, "Hello", mapText(t, m, "t"))

	// Unwinding the original Set must remove the twice-restored copy.
	require.True(t, um.Undo())
	require.False(t, m.Has("t"))
}

// Same, with the base state coming from a remote peer; the restore must also
// converge on that peer.
func TestInteg_UndoManager_DeletedNestedText_RemoteBase(t *testing.T) {
	docA := newTestDoc(1)
	mA := docA.GetMap("m")
	docA.Transact(func(txn *Transaction) {
		txt := NewTextPrelim()
		txt.Insert(txn, 0, "Hello", nil)
		mA.Set(txn, "t", txt)
	})

	docB := newTestDoc(2)
	mB := docB.GetMap("m")
	syncTo(t, docA, docB)
	um := NewUndoManager(docB, []SharedType{mB})
	docB.Transact(func(txn *Transaction) { mB.Delete(txn, "t") })

	require.True(t, um.Undo())
	require.Equal(t, "Hello", mapText(t, mB, "t"))
	syncTo(t, docB, docA)
	require.Equal(t, "Hello", mapText(t, mA, "t"))
}

// Array element holding a map whose values are themselves nested types.
func TestUnit_UndoManager_DeletedNestedInNested_RestoresChildren(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	um := NewUndoManager(doc, []SharedType{arr})

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"x"})
		inner := NewMapPrelim()
		inner.Set(txn, "k", "v")
		src := NewTextPrelim()
		src.Insert(txn, 0, "deep", nil)
		inner.Set(txn, "src", src)
		sub := NewArrayPrelim()
		sub.Push(txn, []any{1, 2})
		inner.Set(txn, "sub", sub)
		arr.PushType(txn, inner)
		arr.Push(txn, []any{"y"})
	})
	want, err := arr.ToJSON()
	require.NoError(t, err)
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 1, 1) })
	require.Equal(t, 2, arr.Len())

	for i := 0; i < 2; i++ {
		require.True(t, um.Undo())
		got, err := arr.ToJSON()
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
		if i == 0 {
			require.True(t, um.Redo())
			require.Equal(t, 2, arr.Len())
		}
	}
}

// A split must carry the redone link to the right half at the same offset, and
// a merge must not swallow it (Yjs splitItem / mergeWith parity).
func TestUnit_Item_SplitAndMerge_PreserveRedone(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "Hello", nil) })
	orig := doc.store.clients[1][0]
	orig.redone = &ID{Client: 7, Clock: 40}

	var right *Item
	doc.Transact(func(txn *Transaction) {
		right = doc.store.getItemCleanStart(txn, ID{Client: 1, Clock: 2})
	})
	require.NotNil(t, right.redone)
	require.Equal(t, ID{Client: 7, Clock: 42}, *right.redone)
	require.Same(t, right, orig.Right, "commit must not re-merge redone halves")
}
