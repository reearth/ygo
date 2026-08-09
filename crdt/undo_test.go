package crdt

import (
	"context"
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

func TestUnit_UndoManager_WithYArray_UndoRedo(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	um := NewUndoManager(doc, []sharedType{arr})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x", "y"}) })
	assert.Equal(t, 2, arr.Len())

	ok := um.Undo()
	require.True(t, ok)
	assert.Equal(t, 0, arr.Len())

	ok = um.Redo()
	require.True(t, ok)
	assert.Equal(t, 2, arr.Len())
}

func TestUnit_UndoManager_WithYMap_UndoRedo(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	um := NewUndoManager(doc, []sharedType{m})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", "v") })
	v, ok := m.Get("k")
	assert.True(t, ok)
	assert.Equal(t, "v", v)

	undone := um.Undo()
	require.True(t, undone)
	_, ok = m.Get("k")
	assert.False(t, ok)

	redone := um.Redo()
	require.True(t, redone)
	v, ok = m.Get("k")
	assert.True(t, ok)
	assert.Equal(t, "v", v)
}

// ---------------------------------------------------------------------------
// Context-aware methods (#27)
// ---------------------------------------------------------------------------

func TestUndoManager_UndoContext_PreCancelledReturnsCtxErr(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	mgr := NewUndoManager(doc, []sharedType{arr})
	defer mgr.Destroy()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	did, err := mgr.UndoContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, did, "Undo must not run when ctx pre-cancelled")
}

func TestUndoManager_UndoContext_OkReturnsResult(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	mgr := NewUndoManager(doc, []sharedType{arr})
	defer mgr.Destroy()

	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 0, []any{"x"}) })

	did, err := mgr.UndoContext(context.Background())
	require.NoError(t, err)
	assert.True(t, did, "Undo should succeed with one item on the stack")
}

func TestUndoManager_RedoContext_PreCancelledReturnsCtxErr(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	mgr := NewUndoManager(doc, []sharedType{arr})
	defer mgr.Destroy()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	did, err := mgr.RedoContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, did, "Redo must not run when ctx pre-cancelled")
}

func TestUndoManager_RedoContext_OkReturnsResult(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	mgr := NewUndoManager(doc, []sharedType{arr})
	defer mgr.Destroy()

	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 0, []any{"x"}) })
	mgr.Undo() // put something on the redo stack

	did, err := mgr.RedoContext(context.Background())
	require.NoError(t, err)
	assert.True(t, did, "Redo should succeed with one item on the redo stack")
}

// zeroTok is a named zero-size type: a pointer to it can NOT serve as a
// unique per-instance token (all such pointers may share runtime.zerobase),
// which is exactly what WithTrackedOrigins must refuse (#203).
type zeroTok struct{}

// undoTok is the safe spelling the godoc recommends: the _ byte field makes
// every allocation distinct, so pointer identity is real.
type undoTok struct{ _ byte }

// THE #203 GUARD: WithTrackedOrigins must reject pointer-to-zero-size origin
// tokens at construction time. Go satisfies every zero-size allocation from
// runtime.zerobase, so two `new(struct{})` tokens compare ==, and a caller
// who tracks one silently captures the other's transactions too — the same
// aliasing that disabled relay publish for six releases inside
// provider/websocket (see relayOriginSentinel's doc). The library cannot
// give caller-supplied values distinct types after the fact, so the only
// safe move is to refuse the un-distinguishable shape loudly.
func TestUnit_UndoManager_WithTrackedOrigins_RejectsZeroSizePointerTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin any
	}{
		{"new(struct{})", new(struct{})},
		{"&struct{}{}", &struct{}{}},
		{"pointer to named empty struct", &zeroTok{}},
		{"pointer to zero-size array", new([0]int)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			txt := doc.GetText("t")
			defer func() {
				if recover() == nil {
					t.Fatalf("WithTrackedOrigins accepted %s, a pointer to a zero-size type; "+
						"want a construction-time panic — pointer identity for zero-size types is a lie (runtime.zerobase)", tc.name)
				}
			}()
			um := NewUndoManager(doc, []sharedType{txt}, WithTrackedOrigins(tc.origin))
			um.Destroy() // not reached
		})
	}
}

// The shapes the godoc points callers at must keep working, and must actually
// be distinguishable: tracking one token never captures another's txns.
func TestUnit_UndoManager_WithTrackedOrigins_DistinguishableTokenShapes(t *testing.T) {
	t.Run("non-zero-size pointer tokens are distinct per allocation", func(t *testing.T) {
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		mine, theirs := &undoTok{}, &undoTok{}
		require.NotSame(t, mine, theirs,
			"sanity: non-zero-size allocations must not alias")

		um := NewUndoManager(doc, []sharedType{txt}, WithTrackedOrigins(mine))
		defer um.Destroy()

		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "mine", nil) }, mine)
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 4, " theirs", nil) }, theirs)

		require.Equal(t, 1, um.UndoStackSize(), "only the tracked token's txn may be captured")
		require.True(t, um.Undo())
		assert.Equal(t, " theirs", txt.ToString())
	})

	t.Run("distinct named zero-size VALUE types are distinguishable by type", func(t *testing.T) {
		// Interface equality compares dynamic type first, so two different
		// named empty-struct types never alias each other — only pointers to
		// them are refused. Values stay legal.
		type originA struct{}
		type originB struct{}
		doc := newTestDoc(1)
		txt := doc.GetText("t")

		um := NewUndoManager(doc, []sharedType{txt}, WithTrackedOrigins(originA{}))
		defer um.Destroy()

		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "a", nil) }, originA{})
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 1, "b", nil) }, originB{})

		require.Equal(t, 1, um.UndoStackSize(), "originB{} must not be captured when tracking originA{}")
		require.True(t, um.Undo())
		assert.Equal(t, "b", txt.ToString())
	})
}
