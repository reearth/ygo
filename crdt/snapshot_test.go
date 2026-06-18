package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Unit tests ────────────────────────────────────────────────────────────────

func TestUnit_CaptureSnapshot_EmptyDoc(t *testing.T) {
	doc := newTestDoc(1)
	snap := CaptureSnapshot(doc)

	assert.Equal(t, StateVector{}, snap.StateVector)
	assert.Empty(t, snap.DeleteSet.clients)
}

func TestUnit_CaptureSnapshot_StateVectorMatchesDoc(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	snap := CaptureSnapshot(doc)
	assert.Equal(t, doc.StateVector(), snap.StateVector)
}

func TestUnit_CaptureSnapshot_DeleteSetIncludesDeleted(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	snap := CaptureSnapshot(doc)
	// All 5 rune positions should appear in the delete set.
	assert.NotEmpty(t, snap.DeleteSet.clients)
	count := uint64(0)
	for _, ranges := range snap.DeleteSet.clients {
		for _, r := range ranges {
			count += r.Len
		}
	}
	assert.Equal(t, uint64(5), count)
}

func TestUnit_EncodeDecodeSnapshot_RoundTrip(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 1, 3) }) // "he_lo" → "ho"

	snap := CaptureSnapshot(doc)
	data := EncodeSnapshot(snap)
	snap2, err := DecodeSnapshot(data)
	require.NoError(t, err)
	assert.True(t, EqualSnapshots(snap, snap2))
}

func TestUnit_EqualSnapshots_Same(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })

	snap := CaptureSnapshot(doc)
	snap2 := CaptureSnapshot(doc) // same state
	assert.True(t, EqualSnapshots(snap, snap2))
}

func TestUnit_EqualSnapshots_DifferentSV(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	snap1 := CaptureSnapshot(doc)

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 3, "xyz", nil) })
	snap2 := CaptureSnapshot(doc)

	assert.False(t, EqualSnapshots(snap1, snap2))
}

func TestUnit_EqualSnapshots_DifferentDS(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcde", nil) })
	snap1 := CaptureSnapshot(doc)

	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 1) })
	snap2 := CaptureSnapshot(doc)

	// Same SV but different DS.
	assert.False(t, EqualSnapshots(snap1, snap2))
}

func TestUnit_RunGC_NoOp_WhenGCDisabled(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	RunGC(doc)

	// With GC off, content should still be ContentString, not ContentDeleted.
	for _, items := range doc.store.clients {
		for _, item := range items {
			if item.Deleted {
				_, isCD := item.Content.(*ContentDeleted)
				assert.False(t, isCD, "GC should not run when disabled")
			}
		}
	}
}

func TestUnit_RunGC_ReplacesDeletedContent(t *testing.T) {
	doc := New(WithClientID(1), WithGC(true))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	RunGC(doc)

	for _, items := range doc.store.clients {
		for _, item := range items {
			if item.Deleted {
				_, isCD := item.Content.(*ContentDeleted)
				assert.True(t, isCD, "deleted item content should be ContentDeleted after GC")
			}
		}
	}
}

// ── Integration tests ─────────────────────────────────────────────────────────

func TestInteg_Snapshot_EmptyDoc_RoundTrip(t *testing.T) {
	doc := newTestDoc(1)
	snap := CaptureSnapshot(doc)

	data := EncodeSnapshot(snap)
	snap2, err := DecodeSnapshot(data)
	require.NoError(t, err)
	assert.True(t, EqualSnapshots(snap, snap2))
}

func TestInteg_RestoreDocument_SimpleText(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello world", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 5, 6) }) // remove " world"

	snap := CaptureSnapshot(doc)
	restored, err := RestoreDocument(doc, snap)
	require.NoError(t, err)
	assert.Equal(t, "hello", restored.GetText("t").ToString())
}

func TestInteg_RestoreDocument_AtPastState(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	snapBefore := CaptureSnapshot(doc)

	// Add more content after the snapshot.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 5, " world", nil) })
	assert.Equal(t, "hello world", txt.ToString())

	// Restore to the earlier state — should only contain "hello".
	restored, err := RestoreDocument(doc, snapBefore)
	require.NoError(t, err)
	assert.Equal(t, "hello", restored.GetText("t").ToString())
}

func TestInteg_RestoreDocument_YMap(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "a", "alpha")
		m.Set(txn, "b", "beta")
	})
	snapFull := CaptureSnapshot(doc)

	// Delete key "a" after snapshot.
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "a") })

	// Current state: only "b".
	_, ok := m.Get("a")
	assert.False(t, ok)

	// Restored state: both keys present.
	restored, err := RestoreDocument(doc, snapFull)
	require.NoError(t, err)
	rm := restored.GetMap("m")
	va, ok := rm.Get("a")
	require.True(t, ok)
	assert.Equal(t, "alpha", va)
	vb, ok := rm.Get("b")
	require.True(t, ok)
	assert.Equal(t, "beta", vb)
}

func TestInteg_RestoreDocument_YArray(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{1, 2, 3}) })
	snap := CaptureSnapshot(doc)

	// Append more elements after snapshot.
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{4, 5}) })
	assert.Equal(t, 5, arr.Len())

	// Restored should have only [1, 2, 3].
	restored, err := RestoreDocument(doc, snap)
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, restored.GetArray("a").ToSlice())
}

func TestInteg_EncodeStateFromSnapshot(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "original", nil) })
	snap := CaptureSnapshot(doc)

	// Add more content after snapshot.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 8, " extra", nil) })

	// Encode the state at snapshot time.
	update, err := EncodeStateFromSnapshot(doc, snap)
	require.NoError(t, err)

	// Apply to a new doc — should reproduce state at snapshot time.
	newDoc := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(newDoc, update, nil))
	assert.Equal(t, "original", newDoc.GetText("t").ToString())
}

func TestInteg_RunGC_DocStillFunctional(t *testing.T) {
	doc := New(WithClientID(1), WithGC(true))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello world", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 5, 6) }) // "hello"

	RunGC(doc)

	// After GC, the doc should still read correctly.
	assert.Equal(t, "hello", txt.ToString())
	assert.Equal(t, 5, txt.Len())

	// And new inserts should still work.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 5, "!", nil) })
	assert.Equal(t, "hello!", txt.ToString())
}

func TestInteg_RunGC_UpdateRoundTrip(t *testing.T) {
	doc := New(WithClientID(1), WithGC(true))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello world", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 5, 6) })

	RunGC(doc)

	// Encode and apply to new doc — should preserve visible content.
	update := EncodeStateAsUpdateV1(doc, nil)
	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil))
	assert.Equal(t, "hello", doc2.GetText("t").ToString())
}

func TestInteg_Snapshot_MultipleClients_Convergence(t *testing.T) {
	doc1 := New(WithClientID(1), WithGC(false))
	doc2 := New(WithClientID(2), WithGC(false))

	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "Alice", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "Bob", nil) })

	// Sync both docs.
	u1 := EncodeStateAsUpdateV1(doc1, nil)
	u2 := EncodeStateAsUpdateV1(doc2, nil)
	require.NoError(t, ApplyUpdateV1(doc1, u2, nil))
	require.NoError(t, ApplyUpdateV1(doc2, u1, nil))
	assert.Equal(t, doc1.GetText("t").ToString(), doc2.GetText("t").ToString())

	// Snapshot after sync — both docs should produce equal snapshots.
	snap1 := CaptureSnapshot(doc1)
	snap2 := CaptureSnapshot(doc2)
	assert.True(t, EqualSnapshots(snap1, snap2))
}

// ── CreateDocFromSnapshot (#58) ─────────────────────────────────────────────

// Yjs-parity reconstruction: rebuild a historic doc from a snapshot taken
// before later mutations.
func TestInteg_CreateDocFromSnapshot_AtPastState(t *testing.T) {
	src := New(WithClientID(1), WithGC(false))
	txt := src.GetText("t")
	src.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	snap := CaptureSnapshot(src)

	src.Transact(func(txn *Transaction) { txt.Insert(txn, 5, " world", nil) })
	require.Equal(t, "hello world", txt.ToString())

	got, err := CreateDocFromSnapshot(src, snap)
	require.NoError(t, err)
	assert.Equal(t, "hello", got.GetText("t").ToString())
}

// A key deleted AFTER the snapshot must reappear in the reconstruction — this
// is the case that silently breaks when the source was GC'd (content of the
// post-snapshot-deleted item is gone), which the guard below prevents.
func TestInteg_CreateDocFromSnapshot_YMapDeletedKeyReappears(t *testing.T) {
	src := New(WithClientID(1), WithGC(false))
	m := src.GetMap("m")
	src.Transact(func(txn *Transaction) {
		m.Set(txn, "a", "alpha")
		m.Set(txn, "b", "beta")
	})
	snap := CaptureSnapshot(src)
	src.Transact(func(txn *Transaction) { m.Delete(txn, "a") })

	got, err := CreateDocFromSnapshot(src, snap)
	require.NoError(t, err)
	va, ok := got.GetMap("m").Get("a")
	require.True(t, ok, "key deleted after the snapshot must reappear in the reconstruction")
	assert.Equal(t, "alpha", va)
}

// A GC-enabled source may have discarded the content/tombstones the snapshot
// references, so reconstruction can't be faithful — CreateDocFromSnapshot must
// refuse rather than return a silently-wrong doc.
func TestUnit_CreateDocFromSnapshot_ErrorsOnGCEnabledSource(t *testing.T) {
	src := New(WithClientID(1)) // gc defaults to true
	txt := src.GetText("t")
	src.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	snap := CaptureSnapshot(src)

	got, err := CreateDocFromSnapshot(src, snap)
	require.ErrorIs(t, err, ErrSnapshotSourceGCed)
	assert.Nil(t, got)
}

// The reconstructed doc is independent of the source and is itself non-GC, so
// it can be snapshotted/restored further.
func TestInteg_CreateDocFromSnapshot_ReconstructedIsIndependentAndRestorable(t *testing.T) {
	src := New(WithClientID(1), WithGC(false))
	txt := src.GetText("t")
	src.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "base", nil) })
	snap := CaptureSnapshot(src)

	got, err := CreateDocFromSnapshot(src, snap)
	require.NoError(t, err)

	// Mutating the reconstruction must not touch the source. (Resolve the text
	// ref outside Transact — GetText inside the closure would deadlock on the
	// doc lock.)
	gotTxt := got.GetText("t")
	got.Transact(func(txn *Transaction) { gotTxt.Insert(txn, 4, "X", nil) })
	assert.Equal(t, "base", txt.ToString(), "source unaffected by edits to the reconstruction")
	assert.Equal(t, "baseX", gotTxt.ToString())

	// Reconstruction is non-GC: it can be snapshotted again without losing history.
	assert.False(t, got.gc)
	snap2 := CaptureSnapshot(got)
	assert.NotNil(t, snap2)
}

// RestoreDocument now delegates to CreateDocFromSnapshot, so it gains the same
// GC-safety guard instead of silently returning a wrong doc.
func TestUnit_RestoreDocument_ErrorsOnGCEnabledSource(t *testing.T) {
	src := New(WithClientID(1)) // gc defaults to true
	txt := src.GetText("t")
	src.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hi", nil) })
	snap := CaptureSnapshot(src)

	got, err := RestoreDocument(src, snap)
	require.ErrorIs(t, err, ErrSnapshotSourceGCed)
	assert.Nil(t, got)
}
