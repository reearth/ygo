package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for #78 — transaction-end housekeeping. H1 (auto-GC) tombstones
// deleted items' content with ContentDeleted (length-only placeholder) so
// long-running documents don't leak memory. H2 (split re-merge) reverses
// unnecessary item splits to reduce linked-list fragmentation.

// findItemByContentType returns the first item whose content matches the
// given Go type, walking all clients. Used by tests that need to inspect
// a specific item across the auto-GC boundary.
func findItemByContentType(doc *Doc, typeAssert func(Content) bool) *Item {
	for _, items := range doc.store.clients {
		for _, item := range items {
			if typeAssert(item.Content) {
				return item
			}
		}
	}
	return nil
}

// H1 — When doc.GC is true (default), transaction commit replaces the
// content of items tombstoned in this transaction with ContentDeleted.
func TestUnit_Transaction_AutoGC_FreesDeletedContent(t *testing.T) {
	doc := New(WithGC(true), WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	// Find the "hello" item — it's a ContentString of length 5.
	helloItem := findItemByContentType(doc, func(c Content) bool {
		_, ok := c.(*ContentString)
		return ok
	})
	require.NotNil(t, helloItem, "ContentString item must exist after insert")
	originalLen := helloItem.Content.Len()
	require.Equal(t, 5, originalLen)

	// Delete in a separate transaction → auto-GC kicks in at commit.
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	assert.True(t, helloItem.Deleted, "item must be tombstoned")
	_, isContentDeleted := helloItem.Content.(*ContentDeleted)
	assert.True(t, isContentDeleted,
		"auto-GC must replace deleted ContentString with ContentDeleted")
	assert.Equal(t, originalLen, helloItem.Content.Len(),
		"the tombstone preserves the original length for clock accounting")
}

// H1 — When doc.GC is false, the content stays intact (so RestoreDocument
// can still reconstruct pre-deletion state).
func TestUnit_Transaction_NoAutoGC_WhenGCDisabled(t *testing.T) {
	doc := New(WithGC(false), WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	helloItem := findItemByContentType(doc, func(c Content) bool {
		_, ok := c.(*ContentString)
		return ok
	})
	require.NotNil(t, helloItem)

	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	assert.True(t, helloItem.Deleted)
	cs, isContentString := helloItem.Content.(*ContentString)
	require.True(t, isContentString,
		"with GC disabled, content must stay as the original ContentString")
	assert.Equal(t, "hello", cs.Str, "the original text remains available for restoration")
}

// H1 — Observer events still see the correct Delete delta after auto-GC.
// Auto-GC runs after buildPhase2 (after deltas are computed), so the
// content captured in the Delta is the original — not ContentDeleted.
func TestUnit_Transaction_AutoGC_DoesNotCorruptDeltas(t *testing.T) {
	doc := New(WithGC(true), WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	var observed YTextEvent
	txt.Observe(func(e YTextEvent) { observed = e })

	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	// The Delta must report Delete(5), not zero — auto-GC must not run
	// before delta computation.
	var totalDelete int
	for _, d := range observed.Delta {
		if d.Op == DeltaOpDelete {
			totalDelete += d.Delete
		}
	}
	assert.Equal(t, 5, totalDelete,
		"observer Delta must capture the original deletion length, not the GC'd tombstone")
}

// countItemsForClient returns the number of items in the store for the given
// client. Used by H2 tests to check whether two halves were re-merged into one.
func countItemsForClient(doc *Doc, client ClientID) int {
	return len(doc.store.clients[client])
}

// H2 — directly exercise splitItem + tryMergeWithLefts via the store
// helpers. Forces a clean split, then verifies it is reversed when no item is
// inserted between the halves before transaction commit.
func TestUnit_Transaction_H2_DirectSplitRemerge(t *testing.T) {
	doc := New(WithGC(false), WithClientID(1))
	txt := doc.GetText("t")

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcdef", nil) })
	require.Equal(t, 1, countItemsForClient(doc, 1))

	// Force a split via getItemCleanEnd, which calls splitItem under the hood.
	// No item is integrated between the halves, so tryMergeWithLefts must
	// reverse the split at commit.
	doc.Transact(func(txn *Transaction) {
		_ = doc.store.getItemCleanEnd(txn, 1, 2) // split after clock 2
	})

	assert.Equal(t, 1, countItemsForClient(doc, 1),
		"H2 must collapse a split that left no item between the halves")
	assert.Equal(t, "abcdef", txt.ToString(),
		"content must be intact after re-merge")
}

// H2 — when an item IS inserted between the halves, the split must stay.
// This guards against a regression where tryMergeWithLefts merges blindly.
func TestUnit_Transaction_H2_DoesNotMergeWhenItemInserted(t *testing.T) {
	docA := New(WithGC(false), WithClientID(1))
	docB := New(WithGC(false), WithClientID(2))
	txtA := docA.GetText("t")

	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, 0, "abcdef", nil) })

	// Sync A → B so B sees the original 6-character run.
	require.NoError(t, ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, nil), nil))
	txtB := docB.GetText("t")

	// B inserts "X" between b and c. On A, when the update applies, the run
	// gets split AND an item ends up between the halves — H2 must NOT merge.
	docB.Transact(func(txn *Transaction) { txtB.Insert(txn, 2, "X", nil) })

	require.NoError(t, ApplyUpdateV1(docA, EncodeStateAsUpdateV1(docB, nil), nil))

	assert.Equal(t, "abXcdef", txtA.ToString(),
		"content must reflect the foreign insertion")
	// Client 1 (A's original) should have at least two items now: clocks 0-1
	// (the "ab" half) and clocks 2-5 (the "cdef" half). H2 must leave both
	// in place because client 2's "X" sits between them in the linked list.
	assert.GreaterOrEqual(t, countItemsForClient(docA, 1), 2,
		"split must stay intact when a foreign item sits between the halves")
}

// H2 — UndoManager attachment must suppress H1 auto-GC so that
// applyStackItem can restore deleted items. This is the regression test for
// the integration discovered during H2 implementation.
func TestUnit_Transaction_H1_SuppressedWhenUndoManagerAttached(t *testing.T) {
	doc := New(WithGC(true), WithClientID(1))
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	helloItem := findItemByContentType(doc, func(c Content) bool {
		_, ok := c.(*ContentString)
		return ok
	})
	require.NotNil(t, helloItem)

	// Delete in a separate transaction. With an UndoManager attached, H1
	// must NOT run — otherwise the content gets tombstoned and undo can't
	// restore "hello".
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	_, isContentDeleted := helloItem.Content.(*ContentDeleted)
	assert.False(t, isContentDeleted,
		"H1 must be suppressed while an UndoManager is attached, so undo can restore content")
	cs, ok := helloItem.Content.(*ContentString)
	require.True(t, ok)
	assert.Equal(t, "hello", cs.Str)

	// And undo must work.
	require.True(t, um.Undo())
	assert.Equal(t, "hello", txt.ToString())
}

// H2 — once all UndoManagers are destroyed, auto-GC resumes for subsequent
// transactions.
func TestUnit_Transaction_H1_ResumesAfterUndoManagerDestroyed(t *testing.T) {
	doc := New(WithGC(true), WithClientID(1))
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	um.Destroy()

	// After Destroy, the counter is 0 again — H1 resumes.
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	helloItem := findItemByContentType(doc, func(c Content) bool {
		_, ok := c.(*ContentDeleted)
		return ok
	})
	require.NotNil(t, helloItem,
		"once the UndoManager is destroyed, H1 must tombstone deleted content again")
}
