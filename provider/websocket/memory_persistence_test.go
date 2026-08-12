package websocket_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

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

// blockingCompactStore wraps a real persistence.MemoryPersistence and parks
// the FIRST call to Compact until released, immediately BEFORE the actual
// fold runs. It is the seam that makes deterministic (non-flaky) the race
// the PR #230 review flagged at server.go:435: a concurrent StoreUpdate can
// append and increment websocket.MemoryPersistence's pending[room]
// bookkeeping in the window between Compact snapshotting that count and the
// fold it guards actually completing. Only Compact is overridden; every
// other VersionedPersistence method (AppendUpdate included) delegates
// straight to the embedded store, unblocked, so a concurrent StoreUpdate
// proceeds while the fold is parked.
//
// A CAS flag, not sync.Once: Once.Do blocks a second concurrent caller until
// the first call's f returns, which this type's only caller in this test
// (websocket.MemoryPersistence.Compact, called at most once here) never
// triggers — but a CAS costs nothing extra and is the correct primitive for
// "block the first caller only" if a test is ever extended to call Compact
// more than once concurrently.
type blockingCompactStore struct {
	*persistence.MemoryPersistence
	entered chan struct{}
	proceed chan struct{}
	parked  atomic.Bool
}

func newBlockingCompactStore() *blockingCompactStore {
	return &blockingCompactStore{
		MemoryPersistence: persistence.NewMemoryPersistence(),
		entered:           make(chan struct{}),
		proceed:           make(chan struct{}),
	}
}

func (b *blockingCompactStore) Compact(ctx context.Context, room string, keep int) (int, error) {
	if b.parked.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.proceed
	}
	return b.MemoryPersistence.Compact(ctx, room, keep)
}

// A write that lands while a fold is parked mid-flight must still count
// toward the NEXT threshold: Compact must not delete the whole pending[room]
// entry just because IT triggered the fold, only subtract what it
// snapshotted before folding (PR #230 review, server.go:435). Deleting it —
// the pre-fix behaviour — erases the concurrent write's contribution, so the
// un-folded record count can exceed CompactEvery indefinitely once writes
// stop.
//
// CompactEvery is set high so StoreUpdate's own threshold check never fires:
// the fold below is driven by an explicit p.Compact call instead. That
// matters here — the concurrent write's own pending++ would otherwise also
// cross the (low) threshold and recurse into a second, nested Compact call
// on the same room while the first is still parked, folding the concurrent
// write itself and making this test assert on the wrong mechanism.
func TestUnit_MemoryPersistence_CompactPreservesConcurrentPendingIncrement(t *testing.T) {
	store := newBlockingCompactStore()
	adapter := persistence.NewLegacyAdapter(store)
	adapter.KeepVersions = 1 // matches websocket.NewMemoryPersistence's own setup
	p := ygws.NewMemoryPersistenceForTest(adapter)
	p.CompactEvery = 1_000_000 // never self-compact; drive the fold explicitly below

	storeN(t, p, "room", 3) // pending["room"] == 3, no auto-compact at this threshold

	compactDone := make(chan error, 1)
	go func() { compactDone <- p.Compact(context.Background(), "room") }()

	select {
	case <-store.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the explicit Compact never reached the wrapped store")
	}

	// A concurrent write lands while that fold is parked — after Compact has
	// snapshotted pending["room"] but before the fold it guards has run. It
	// must be a well-formed V1 update (a fresh doc's own insert) so the fold
	// that eventually runs can still succeed.
	concurrentDoc := crdt.New()
	concurrentTxt := concurrentDoc.GetText("t")
	concurrentDoc.Transact(func(txn *crdt.Transaction) { concurrentTxt.Insert(txn, 0, "b", nil) })
	require.NoError(t, p.StoreUpdate("room", crdt.EncodeStateAsUpdateV1(concurrentDoc, nil)))

	close(store.proceed) // release the parked fold

	select {
	case err := <-compactDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the parked Compact never returned")
	}

	require.Equal(t, 1, ygws.MemoryPersistenceRecordCount(p, "room"),
		"the fold must still succeed and collapse the room to one record")
	require.Equal(t, 1, ygws.MemoryPersistencePendingCount(p, "room"),
		"the concurrent write's increment must survive: Compact snapshots the "+
			"pending count before folding and must subtract only that snapshot, "+
			"not delete the entry outright, or this write stops contributing "+
			"toward the next threshold (PR #230 review, server.go:435)")
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
			"nothing to abort, so implementing it would switch the server onto the "+
			"cancellable-ctx path purely to gain a cancellation it can only act on "+
			"by discarding the write (persistence.MemoryPersistence.AppendUpdate "+
			"returns ctx.Err() at entry). #229 has since made the worker side of "+
			"that safe — final flushes use a background ctx and a cancelled store "+
			"is retained and re-stored — so this is no longer a loss hazard, just "+
			"a pointless code path (#186)")
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
// an adapter with nothing to actually abort gains nothing from cancellation
// and can only respond to it by throwing the write away.
//
// An end-to-end Server.Shutdown-races-concurrent-Apply test was attempted
// first, per this hazard's own writeup, and abandoned at the time: it
// reproduced write loss on BOTH current and pre-#186 code (verified against
// the pre-#186 commit directly, with and without -race), because
// provider/websocket's shutdown drain had its own separate, pre-existing gap —
// it drained persistCh exactly once and returned, so an update landing in the
// channel after that one-shot drain was never persisted, regardless of which
// PersistenceAdapter was behind it. A test built on that race would have
// failed unpredictably even on a correctly-fixed #186 and misreported this
// hazard as unresolved.
//
// That gap is #229, and it is fixed on this branch — see
// persistence_shutdown_test.go, which now DOES assert the end-to-end property
// (a commit during Shutdown reaches the adapter) deterministically, by parking
// the worker rather than racing it. This test is still worth keeping as the
// narrow, isolated statement of the mechanism: no goroutines, no sleeps, no
// scheduling luck — it calls the exact method with an already-cancelled ctx
// and asserts the write is gone, which is why the server must never hand a
// cancelled ctx to a final flush.
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
