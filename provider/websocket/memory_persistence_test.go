package websocket_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
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

// Explicit Compact must be safe on an unknown room and must not lose state on
// a known one.
func TestUnit_MemoryPersistence_CompactIsSafeAndLossless(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	require.NoError(t, p.Compact(context.Background(), "never-seen"))

	want := storeN(t, p, "room", 7)
	require.NoError(t, p.Compact(context.Background(), "room"))
	require.Equal(t, want, docTextAfterReload(t, p, "room"))
}

// Compaction must actually collapse stored records, not merely preserve
// content. With KeepVersions at its 0 default, Compact is a silent no-op that
// still passes every content-level assertion — so assert the record count.
func TestUnit_MemoryPersistence_CompactionActuallyCollapsesRecords(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	p.CompactEvery = 1_000_000 // never self-compact; drive it explicitly
	storeN(t, p, "room", 12)

	require.Greater(t, ygws.MemoryPersistenceRecordCount(p, "room"), 1,
		"precondition: appended updates must be stored as separate records")

	require.NoError(t, p.Compact(context.Background(), "room"))
	require.Equal(t, 1, ygws.MemoryPersistenceRecordCount(p, "room"),
		"Compact must fold the room to a single record; a KeepVersions=0 no-op would leave them all")
}

// The self-compaction threshold must fire, and must DELETE its bookkeeping
// entry rather than zero it — a map keyed by room name that is only zeroed
// grows without bound as rooms churn (idle eviction makes that routine).
func TestUnit_MemoryPersistence_ThresholdCompactsAndDropsBookkeeping(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	p.CompactEvery = 5
	storeN(t, p, "room", 5)

	require.Equal(t, 1, ygws.MemoryPersistenceRecordCount(p, "room"),
		"reaching CompactEvery must trigger the fold")
	require.Zero(t, ygws.MemoryPersistencePendingRooms(p),
		"the pending entry must be deleted after compaction, not zeroed")
}

// Interface satisfaction is load-bearing: the server type-asserts for these at
// runtime, so gaining or losing one silently changes which persistence-worker
// code path runs.
func TestUnit_MemoryPersistence_SatisfiesExpectedInterfaces(t *testing.T) {
	var p any = ygws.NewMemoryPersistence()
	_, isAdapter := p.(ygws.PersistenceAdapter)
	_, isCtx := p.(ygws.PersistenceAdapterContext)
	_, isCompactable := p.(ygws.CompactableAdapter)
	_, isVersionable := p.(ygws.VersionableAdapter)
	require.True(t, isAdapter)
	require.False(t, isCtx,
		"deliberately NOT a PersistenceAdapterContext: this adapter's append has "+
			"nothing to abort, so implementing it would only cost writes — it would "+
			"switch the server onto the cancellable-ctx path, and on the "+
			"coalescing-disabled path the final shutdown drain reuses a ctx a "+
			"separate goroutine cancels concurrently, discarding a still-queued "+
			"committed write with only a log line (measured: 51-151/200 dropped "+
			"across trials, #186)")
	require.True(t, isCompactable)
	require.False(t, isVersionable,
		"deliberately NOT a VersionableAdapter: auto-versioning an in-memory test adapter is scope creep (#186)")
}

// This is the deterministic, isolated reproduction of the ACTUAL mechanism the
// reviewer identified: persistence.MemoryPersistence.AppendUpdate (reached via
// persistence.LegacyAdapter.StoreUpdateContext, exactly the method
// websocket.MemoryPersistence's removed StoreUpdateContext would have
// forwarded to) checks ctx.Err() at entry and discards an otherwise
// well-formed, committed update the instant ctx is already cancelled — with
// no partial write, no retry, nothing but the returned error. This is why
// giving websocket.MemoryPersistence a StoreUpdateContext was the wrong move:
// the server's persistence worker cancels its ctx the moment shutdown begins
// (startPersistenceWorker's doc, persistence.go) with no ordering guarantee
// against "drain and store what's still queued," so any context-aware adapter
// with nothing to actually abort just loses writes for free.
//
// An end-to-end Server.Shutdown-races-concurrent-Apply test was attempted
// first, per this hazard's own writeup, and abandoned: it reproduces write
// loss on BOTH current and pre-#186 code (verified against the pre-#186
// commit directly, with and without -race), because provider/websocket's
// strict-path shutdown drain has its own separate, pre-existing gap — it
// drains persistCh exactly once and returns, so an update landing in the
// channel microseconds after that one-shot drain is never persisted,
// regardless of which PersistenceAdapter is behind it. That gap predates
// #186, is not specific to MemoryPersistence, and is out of scope for this
// fix wave (flagged separately). Any test built on that end-to-end race would
// fail unpredictably even on a correctly-fixed #186, which would misreport
// this hazard as unresolved. This test isolates the ACTUAL mechanism #186
// introduced instead: no goroutines, no sleeps, no scheduling luck — it calls
// the exact vulnerable method with an already-cancelled ctx and asserts the
// write is gone.
func TestUnit_PersistenceLegacyAdapterStoreUpdateContext_DiscardsOnCancelledCtx(t *testing.T) {
	store := persistence.NewMemoryPersistence()
	adapter := persistence.NewLegacyAdapter(store)
	adapter.KeepVersions = 1 // matches websocket.NewMemoryPersistence's own setup

	doc := crdt.New()
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })
	update := crdt.EncodeStateAsUpdateV1(doc, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate the shutdown drain observing an already-cancelled ctx

	err := adapter.StoreUpdateContext(ctx, "room", update)
	require.Error(t, err, "a context-aware store must refuse an already-cancelled ctx")
	require.ErrorIs(t, err, context.Canceled)

	blob, loadErr := adapter.LoadDoc("room")
	require.NoError(t, loadErr)
	require.Empty(t, blob,
		"the update must NOT have been stored — this is the write-loss mechanism "+
			"#186 measured (51-151/200 dropped over 20 trials) when "+
			"websocket.MemoryPersistence exposed StoreUpdateContext; it now only "+
			"implements the ctx-ignoring PersistenceAdapter, so the persistence "+
			"worker never reaches this method for this adapter (see "+
			"TestUnit_MemoryPersistence_SatisfiesExpectedInterfaces)")
}

// The zero value of MemoryPersistence (e.g. &MemoryPersistence{CompactEvery:
// 2000}, natural now that CompactEvery is an exported field inviting a
// composite literal) has a nil internal adapter. Every method must report
// that clearly rather than nil-dereferencing — the server's persistence
// worker recovers panics from StoreUpdate, so an unguarded nil deref would
// present as total silent write loss with nothing but a log line, not a
// crash that points at the real cause.
func TestUnit_MemoryPersistence_ZeroValueReturnsErrorNotPanic(t *testing.T) {
	p := &ygws.MemoryPersistence{CompactEvery: 2000}

	_, err := p.LoadDoc("room")
	require.Error(t, err, "LoadDoc on the zero value must error, not panic")

	err = p.StoreUpdate("room", []byte("x"))
	require.Error(t, err, "StoreUpdate on the zero value must error, not panic")

	err = p.Compact(context.Background(), "room")
	require.Error(t, err, "Compact on the zero value must error, not panic")
}
