package crdt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runWithin reports whether fn returned before d elapsed. A callback that
// re-enters the UndoManager used to block forever holding u.mu, so a timeout
// here means a deadlock, not slowness.
func runWithin(t *testing.T, d time.Duration, fn func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// An OnStackItemAdded callback may use the UndoManager and the document that
// fired it. Yjs fires this event with no lock held, so a listener can read the
// stacks or edit the document; ygo must not deadlock on its own mutex.
func TestUnit_UndoManager_CallbackReentersFromCapture(t *testing.T) {
	cases := []struct {
		name string
		act  func(doc *Doc, um *UndoManager, txt *YText)
	}{
		{"UndoStackSize", func(_ *Doc, um *UndoManager, _ *YText) { um.UndoStackSize() }},
		{"RedoStackSize", func(_ *Doc, um *UndoManager, _ *YText) { um.RedoStackSize() }},
		{"StopCapturing", func(_ *Doc, um *UndoManager, _ *YText) { um.StopCapturing() }},
		{"Clear", func(_ *Doc, um *UndoManager, _ *YText) { um.Clear() }},
		{"OnStackItemAdded", func(_ *Doc, um *UndoManager, _ *YText) {
			um.OnStackItemAdded(func(*StackItem, bool) {})
		}},
		{"TransactInScope", func(doc *Doc, _ *UndoManager, txt *YText) {
			doc.Transact(func(tx *Transaction) { txt.Insert(tx, 0, "x", nil) })
		}},
		{"TransactOutOfScope", func(doc *Doc, _ *UndoManager, _ *YText) {
			other := doc.GetText("other")
			doc.Transact(func(tx *Transaction) { other.Insert(tx, 0, "x", nil) })
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := New()
			txt := doc.GetText("t")
			um := NewUndoManager(doc, []SharedType{txt})
			t.Cleanup(um.Destroy)

			fired := false
			um.OnStackItemAdded(func(*StackItem, bool) {
				if fired {
					return
				}
				fired = true
				tc.act(doc, um, txt)
			})

			ok := runWithin(t, 5*time.Second, func() {
				doc.Transact(func(tx *Transaction) { txt.Insert(tx, 0, "hello", nil) })
			})
			require.True(t, ok, "callback re-entering the UndoManager deadlocked")
			require.True(t, fired, "callback never ran")
		})
	}
}

// The same callback also fires from pop, on the undo and redo paths.
func TestUnit_UndoManager_CallbackReentersFromPop(t *testing.T) {
	doc := New()
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []SharedType{txt})
	t.Cleanup(um.Destroy)

	doc.Transact(func(tx *Transaction) { txt.Insert(tx, 0, "hello", nil) })

	var sizes []int
	um.OnStackItemAdded(func(*StackItem, bool) { sizes = append(sizes, um.UndoStackSize()) })

	require.True(t, runWithin(t, 5*time.Second, func() { um.Undo() }),
		"callback fired from Undo deadlocked")
	require.True(t, runWithin(t, 5*time.Second, func() { um.Redo() }),
		"callback fired from Redo deadlocked")
	require.NotEmpty(t, sizes, "callback never ran from pop")
}

// Registering a callback while captures are firing must not race: the firing
// goroutine reads the slice header under u.mu and iterates it unlocked, while
// OnStackItemAdded appends under u.mu.
func TestInteg_UndoManager_RegisterWhileFiring(t *testing.T) {
	doc := New()
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []SharedType{txt})
	t.Cleanup(um.Destroy)
	um.OnStackItemAdded(func(*StackItem, bool) {})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			um.OnStackItemAdded(func(*StackItem, bool) {})
		}
	}()
	for i := 0; i < 200; i++ {
		um.StopCapturing() // force a new stack item each time
		doc.Transact(func(tx *Transaction) { txt.Insert(tx, 0, "x", nil) })
	}
	<-done
	require.Positive(t, um.UndoStackSize())
}
