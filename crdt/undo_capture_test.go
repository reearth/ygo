package crdt

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mergeWindow keeps every transaction in a test inside one capture interval.
var mergeWindow = WithCaptureTimeout(time.Hour)

func arrJSON(t *testing.T, a *YArray) string {
	t.Helper()
	b, err := a.ToJSON()
	require.NoError(t, err)
	return string(b)
}

// An edit to a type nested inside a tracked type is captured (Yjs
// changedParentTypes), so undo restores the value it overwrote.
func TestUnit_UndoManager_CapturesNestedEdit(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		m := NewMapPrelim()
		m.Set(txn, "k", 1)
		arr.PushType(txn, m)
	})
	m := arr.Get(0).(*YMap)
	um := NewUndoManager(doc, []SharedType{arr}, mergeWindow)

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{5}) })
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", 2) })
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{6}) })
	require.Equal(t, 1, um.UndoStackSize())

	require.True(t, um.Undo())
	require.JSONEq(t, `[{"k":1}]`, arrJSON(t, arr))
	require.True(t, um.Redo())
	require.JSONEq(t, `[{"k":2},5,6]`, arrJSON(t, arr))
}

// A nested-only edit is a stack item of its own.
func TestUnit_UndoManager_CapturesNestedOnlyEdit(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		txt := NewTextPrelim()
		txt.Insert(txn, 0, "ab", nil)
		arr.PushType(txn, txt)
	})
	txt := arr.Get(0).(*YText)
	um := NewUndoManager(doc, []SharedType{arr})

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 2, "c", nil) })
	require.Equal(t, 1, um.UndoStackSize())
	require.True(t, um.Undo())
	require.Equal(t, "ab", txt.ToString())
}

// A client absent from the first transaction's beforeState starts at clock 0.
func TestUnit_UndoManager_MergedFirstEditsOnFreshDoc(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	um := NewUndoManager(doc, []SharedType{arr}, mergeWindow)

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x"}) })
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"y"}) })
	require.True(t, um.Undo())
	require.Equal(t, 0, arr.Len())
}

// An item inserted and deleted inside one capture interval is not resurrected
// by undo (Yjs popStackItem skips deletions that are also insertions).
func TestUnit_UndoManager_InsertThenDeleteInOneItem(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"base"}) })
	um := NewUndoManager(doc, []SharedType{arr}, mergeWindow)

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x"}) })
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 1, 1) })
	require.False(t, um.Undo(), "the only stack item is a no-op")
	require.JSONEq(t, `["base"]`, arrJSON(t, arr))
}

// Undo discards no-op stack items and applies the next one (Yjs popStackItem).
func TestUnit_UndoManager_UndoSkipsNoOpItem(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	um := NewUndoManager(doc, []SharedType{arr}, mergeWindow)

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"base"}) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x"}) })
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 1, 1) })
	require.True(t, um.Undo())
	require.Equal(t, 0, arr.Len())
	require.Equal(t, 0, um.UndoStackSize())
	require.Equal(t, 1, um.RedoStackSize())
	require.True(t, um.Redo())
	require.JSONEq(t, `["base"]`, arrJSON(t, arr))
}

// undoPeers returns a tracked doc A and a peer B, both holding base.
func undoPeers(t *testing.T, base func(*Doc)) (a, b *Doc) {
	t.Helper()
	return undoPeersIDs(t, 1, 2, base)
}

func undoPeersIDs(t *testing.T, idA, idB uint64, base func(*Doc)) (a, b *Doc) {
	t.Helper()
	a, b = newTestDoc(idA), newTestDoc(idB)
	base(a)
	syncTo(t, a, b)
	return a, b
}

// peerOrders runs fn with the tracked peer sorting both below and above the
// remote one, since map LWW alone protects the remote value in one order.
func peerOrders(t *testing.T, fn func(t *testing.T, idA, idB uint64)) {
	t.Run("tracked-low", func(t *testing.T) { fn(t, 1, 2) })
	t.Run("tracked-high", func(t *testing.T) { fn(t, 2, 1) })
}

// A remote insert landing between two merged local transactions survives undo.
func TestInteg_UndoManager_KeepsRemoteInsertBetweenMergedLocal(t *testing.T) {
	docA, docB := undoPeers(t, func(d *Doc) {
		d.Transact(func(txn *Transaction) { txn.GetArray("a").Push(txn, []any{"d0"}) })
	})
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
	docB.Transact(func(txn *Transaction) { arrB.Push(txn, []any{"r0"}) })
	syncTo(t, docB, docA)

	um := NewUndoManager(docA, []SharedType{arrA}, mergeWindow)
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"mine1"}) })
	syncTo(t, docA, docB)
	docB.Transact(func(txn *Transaction) { arrB.Push(txn, []any{"theirs"}) })
	syncTo(t, docB, docA)
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"mine2"}) })
	require.Equal(t, 1, um.UndoStackSize())

	require.True(t, um.Undo())
	require.JSONEq(t, `["d0","r0","theirs"]`, arrJSON(t, arrA))
	require.True(t, um.Redo())
	require.JSONEq(t, `["d0","r0","mine1","theirs","mine2"]`, arrJSON(t, arrA))

	syncTo(t, docA, docB)
	syncTo(t, docB, docA)
	require.Equal(t, arrJSON(t, arrA), arrJSON(t, arrB))
}

// A remote edit inside a nested type, between merged local transactions,
// survives undo.
func TestInteg_UndoManager_KeepsRemoteNestedInsert(t *testing.T) {
	docA, docB := undoPeers(t, func(d *Doc) {
		d.Transact(func(txn *Transaction) {
			txt := NewTextPrelim()
			txt.Insert(txn, 0, "ab", nil)
			txn.GetArray("a").PushType(txn, txt)
		})
	})
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
	txtA, txtB := arrA.Get(0).(*YText), arrB.Get(0).(*YText)

	um := NewUndoManager(docA, []SharedType{arrA}, mergeWindow)
	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, 0, "<", nil) })
	syncTo(t, docA, docB)
	docB.Transact(func(txn *Transaction) { txtB.Insert(txn, 2, "R", nil) })
	syncTo(t, docB, docA)
	end := txtA.Len()
	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, end, ">", nil) })
	require.Equal(t, "<aRb>", txtA.ToString())

	require.True(t, um.Undo())
	require.Equal(t, "aRb", txtA.ToString())
}

// A remote delete between merged local transactions is not restored by undo.
func TestInteg_UndoManager_KeepsRemoteDeleteBetweenMergedLocal(t *testing.T) {
	docA, docB := undoPeers(t, func(d *Doc) {
		d.Transact(func(txn *Transaction) { txn.GetArray("a").Push(txn, []any{"p", "q"}) })
	})
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")

	um := NewUndoManager(docA, []SharedType{arrA}, mergeWindow)
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"x"}) })
	syncTo(t, docA, docB)
	docB.Transact(func(txn *Transaction) { arrB.Delete(txn, 0, 1) })
	syncTo(t, docB, docA)
	docA.Transact(func(txn *Transaction) { arrA.Delete(txn, 0, 1) }) // q
	require.JSONEq(t, `["x"]`, arrJSON(t, arrA))

	require.True(t, um.Undo())
	require.JSONEq(t, `["q"]`, arrJSON(t, arrA))
}

// A remote insert BEFORE the first and AFTER the last captured local
// transaction is untouched too, and redo stacks keep working across it.
func TestInteg_UndoManager_RemoteEditsAroundUndoRedo(t *testing.T) {
	docA, docB := undoPeers(t, func(d *Doc) {
		d.Transact(func(txn *Transaction) { txn.GetMap("m").Set(txn, "k", "base") })
	})
	mA, mB := docA.GetMap("m"), docB.GetMap("m")
	um := NewUndoManager(docA, []SharedType{mA}, mergeWindow)

	docA.Transact(func(txn *Transaction) { mA.Set(txn, "a", 1) })
	syncTo(t, docA, docB)
	docB.Transact(func(txn *Transaction) { mB.Set(txn, "b", 2) })
	syncTo(t, docB, docA)
	docA.Transact(func(txn *Transaction) { mA.Set(txn, "c", 3) })

	require.True(t, um.Undo())
	require.True(t, um.Redo())
	syncTo(t, docA, docB)
	docB.Transact(func(txn *Transaction) { mB.Set(txn, "d", 4) })
	syncTo(t, docB, docA)
	require.True(t, um.Undo())
	got, err := mA.ToJSON()
	require.NoError(t, err)
	require.JSONEq(t, `{"k":"base","b":2,"d":4}`, string(got))
}

// Undo does not overwrite a remote peer's later value for the same map key
// (Yjs redoItem's conflict check).
func TestInteg_UndoManager_KeepsRemoteMapOverwrite(t *testing.T) {
	peerOrders(t, func(t *testing.T, idA, idB uint64) {
		docA, docB := undoPeersIDs(t, idA, idB, func(d *Doc) {
			d.Transact(func(txn *Transaction) { txn.GetMap("m").Set(txn, "k", "base") })
		})
		mA, mB := docA.GetMap("m"), docB.GetMap("m")
		um := NewUndoManager(docA, []SharedType{mA})

		docA.Transact(func(txn *Transaction) { mA.Set(txn, "k", "mine") })
		syncTo(t, docA, docB)
		docB.Transact(func(txn *Transaction) { mB.Set(txn, "k", "theirs") })
		syncTo(t, docB, docA)

		um.Undo()
		got, _ := mA.Get("k")
		require.Equal(t, "theirs", got)
	})
}

// Same, for a map nested inside the tracked type.
func TestInteg_UndoManager_KeepsRemoteNestedMapOverwrite(t *testing.T) {
	peerOrders(t, func(t *testing.T, idA, idB uint64) {
		docA, docB := undoPeersIDs(t, idA, idB, func(d *Doc) {
			d.Transact(func(txn *Transaction) {
				m := NewMapPrelim()
				m.Set(txn, "k", 1)
				txn.GetArray("a").PushType(txn, m)
			})
		})
		arrA := docA.GetArray("a")
		mA, mB := arrA.Get(0).(*YMap), docB.GetArray("a").Get(0).(*YMap)
		um := NewUndoManager(docA, []SharedType{arrA})

		docA.Transact(func(txn *Transaction) { mA.Set(txn, "k", 2) })
		syncTo(t, docA, docB)
		docB.Transact(func(txn *Transaction) { mB.Set(txn, "k", 3) })
		syncTo(t, docB, docA)

		um.Undo()
		require.JSONEq(t, `[{"k":3}]`, arrJSON(t, arrA))
	})
}

// A later same-key value deleted by undo history is not a conflict: undoing
// the delete still restores the original value (Yjs isDeletedByUndoStack).
func TestUnit_UndoManager_MapRestorePastUndoneValue(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	um := NewUndoManager(doc, []SharedType{m})
	get := func() any { v, _ := m.Get("k"); return v }

	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", "X") })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "k") })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", "R") })

	require.True(t, um.Undo())
	require.False(t, m.Has("k"))
	require.True(t, um.Undo())
	require.Equal(t, "X", get())
	require.True(t, um.Redo())
	require.False(t, m.Has("k"))
	require.True(t, um.Redo())
	require.Equal(t, "R", get())
}

// Capture merges into the top stack item after the doc unlocks, while a
// concurrent Undo consults the other stack items' deletions; run under -race.
func TestInteg_UndoManager_ConcurrentCaptureAndUndo(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	um := NewUndoManager(doc, []SharedType{m}, WithCaptureTimeout(time.Hour))
	for i := 0; i < 200; i++ {
		doc.Transact(func(txn *Transaction) { m.Set(txn, "k", i) })
		if i%3 == 0 {
			um.StopCapturing()
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			doc.Transact(func(txn *Transaction) {
				m.Set(txn, "k"+strconv.Itoa(i%3), i)
				m.Delete(txn, "k")
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			um.Undo()
			um.Redo()
		}
	}()
	wg.Wait()
}

// Undo stops capturing, so an edit right after it starts a new stack item
// instead of merging into the next-older one (Yjs afterTransactionHandler).
func TestUnit_UndoManager_EditAfterUndoStartsNewItem(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []SharedType{m, txt}, WithCaptureTimeout(time.Hour))
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ba", nil) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ea", nil) })
	require.True(t, um.Undo())
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k2", 14) })

	require.True(t, um.Undo())
	require.Equal(t, "ba", txt.ToString())
	require.False(t, m.Has("k2"))
}

// An edit that commits between an undo's apply and its stack update must
// still invalidate redo, and an edit racing a redo must stay on top of the
// undo stack.
func TestInteg_UndoManager_EditRacingUndoOrRedo(t *testing.T) {
	setup := func() (*Doc, *YArray, *UndoManager, *bool) {
		doc := newTestDoc(1)
		arr := doc.GetArray("a")
		um := NewUndoManager(doc, []SharedType{arr})
		race := new(bool)
		// Runs after the undo/redo transaction commits, before pop relocks u.mu.
		doc.OnAfterTransaction(func(txn *Transaction) {
			if txn.Origin == um && *race {
				*race = false
				doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"edit"}) })
			}
		})
		doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x"}) })
		um.StopCapturing()
		return doc, arr, um, race
	}

	t.Run("undo", func(t *testing.T) {
		_, arr, um, race := setup()
		*race = true
		require.True(t, um.Undo())
		require.Equal(t, []any{"edit"}, arr.ToSlice())
		require.False(t, um.Redo(), "the edit invalidates redo")
		require.Equal(t, []any{"edit"}, arr.ToSlice())
	})

	t.Run("redo", func(t *testing.T) {
		_, arr, um, race := setup()
		require.True(t, um.Undo())
		*race = true
		require.True(t, um.Redo())
		require.Equal(t, []any{"x", "edit"}, arr.ToSlice())
		require.True(t, um.Undo())
		require.Equal(t, []any{"x"}, arr.ToSlice(), "the later edit undoes first")
	})
}
