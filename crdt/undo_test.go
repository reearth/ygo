package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_UndoManager_BasicUndoRedo(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	assert.Equal(t, "hello", txt.ToString())

	ok := um.Undo()
	require.True(t, ok)
	assert.Empty(t, txt.ToString())

	ok = um.Redo()
	require.True(t, ok)
	assert.Equal(t, "hello", txt.ToString())
}

func TestUnit_UndoManager_WithTrackedOrigins_OnlyCapturesMatchingOrigin(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	const userOrigin = "user-alice"
	const remoteOrigin = "peer-bob"

	um := NewUndoManager(doc, []sharedType{txt}, WithTrackedOrigins(userOrigin))
	defer um.Destroy()

	// Local transaction from Alice — should be captured.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "alice", nil) }, userOrigin)
	// Remote transaction from Bob — should NOT be captured.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 5, " bob", nil) }, remoteOrigin)

	assert.Equal(t, "alice bob", txt.ToString())
	assert.Equal(t, 1, um.UndoStackSize(), "only alice's txn should be on the undo stack")

	ok := um.Undo()
	require.True(t, ok)
	// "alice" is removed; " bob" stays (it was not captured).
	assert.Equal(t, " bob", txt.ToString())
}

func TestUnit_UndoManager_WithTrackedOrigins_EmptySetCapturesAll(t *testing.T) {
	// Default UndoManager (no WithTrackedOrigins) captures all local txns.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "a", nil) }, "origin-1")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 1, "b", nil) }, "origin-2")
	// Both should be captured (merged within timeout, so stack size is 1).
	assert.GreaterOrEqual(t, um.UndoStackSize(), 1)
}

func TestUnit_UndoManager_StopCapturing(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "a", nil) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 1, "b", nil) })

	// Two separate stack items because StopCapturing forced a boundary.
	assert.Equal(t, 2, um.UndoStackSize())
}

func TestUnit_UndoManager_OnStackItemAdded(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	var items []*StackItem
	um.OnStackItemAdded(func(item *StackItem, _ bool) {
		items = append(items, item)
	})

	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "x", nil) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 1, "y", nil) })

	assert.Len(t, items, 2, "OnStackItemAdded must fire for each new stack item")
}
