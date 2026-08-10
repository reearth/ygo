package websocket_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// docTextAfterReload applies whatever LoadDoc returns to a fresh Doc and reads
// the "t" text — asserting on CONTENT, never on stored bytes, because the
// storage shape is exactly what this change alters.
func docTextAfterReload(t *testing.T, p *ygws.MemoryPersistence, room string) string {
	t.Helper()
	blob, err := p.LoadDoc(room)
	require.NoError(t, err)
	d := crdt.New()
	if len(blob) > 0 {
		require.NoError(t, crdt.ApplyUpdateV1(d, blob, nil))
	}
	return d.GetText("t").ToString()
}

// storeN writes n single-char updates and returns the expected final text.
func storeN(t *testing.T, p *ygws.MemoryPersistence, room string, n int) string {
	t.Helper()
	doc := crdt.New()
	txt := doc.GetText("t")
	var sv crdt.StateVector
	for i := 0; i < n; i++ {
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, txt.Len(), "a", nil) })
		require.NoError(t, p.StoreUpdate(room, crdt.EncodeStateAsUpdateV1(doc, sv)))
		sv = doc.StateVector()
	}
	return txt.ToString()
}

// Writing far past several CompactEvery boundaries must reload identically to
// writing without crossing any — compaction is a storage detail, never a
// content change (#186).
func TestUnit_MemoryPersistence_RoundTripsAcrossCompactionBoundaries(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	p.CompactEvery = 4
	want := storeN(t, p, "room", 21) // crosses the boundary 5 times
	require.Equal(t, want, docTextAfterReload(t, p, "room"))
}

// The context-aware write path must exist and behave like StoreUpdate: the
// server prefers it, and the wrapped adapter already has it.
func TestUnit_MemoryPersistence_StoreUpdateContextPersists(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	doc := crdt.New()
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })
	require.NoError(t, p.StoreUpdateContext(context.Background(),
		"room", crdt.EncodeStateAsUpdateV1(doc, nil)))
	require.Equal(t, "hi", docTextAfterReload(t, p, "room"))
}

// Explicit Compact must be safe on an unknown room and must not lose state on
// a known one.
func TestUnit_MemoryPersistence_CompactIsSafeAndLossless(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	require.NoError(t, p.Compact(context.Background(), "never-seen"))

	want := storeN(t, p, "room", 7)
	require.NoError(t, p.Compact(context.Background(), "room"))
	require.Equal(t, want, docTextAfterReload(t, p, "room"))
}
