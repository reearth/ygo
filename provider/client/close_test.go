package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// withFlushWriteTimeout and withCloseDrainTimeout replace loop.go's
// package-level flushWriteTimeout/closeDrainTimeout vars with v for the
// duration of the calling test, restoring the real default on cleanup —
// the same "package-level indirection purely for test determinism" pattern
// backoff_test.go's withFixedRand already establishes for randFloat. See
// flushWriteTimeout's own doc for why a test needs to control these
// independently of each other and of the handshake-path writeTimeout const.
func withFlushWriteTimeout(t *testing.T, v time.Duration) {
	t.Helper()
	orig := flushWriteTimeout
	flushWriteTimeout = v
	t.Cleanup(func() { flushWriteTimeout = orig })
}

func withCloseDrainTimeout(t *testing.T, v time.Duration) {
	t.Helper()
	orig := closeDrainTimeout
	closeDrainTimeout = v
	t.Cleanup(func() { closeDrainTimeout = orig })
}

// countingCompactStore wraps a *SQLiteStore and counts both StoreUpdate and
// Compact calls, so a test can assert the client's compaction trigger
// actually fired, and separately compare the total number of writes ever
// made against the row count left afterward — see
// TestClient_CompactionTrigger_DeletesAfterThreshold's doc for why that
// comparison, not a fixed post-compaction row count, is the robust
// assertion here.
type countingCompactStore struct {
	*SQLiteStore
	compactCalls atomic.Int64
	storeCalls   atomic.Int64
}

func (s *countingCompactStore) StoreUpdate(room string, update []byte) error {
	s.storeCalls.Add(1)
	return s.SQLiteStore.StoreUpdate(room, update)
}

func (s *countingCompactStore) Compact(ctx context.Context, room string) error {
	s.compactCalls.Add(1)
	return s.SQLiteStore.Compact(ctx, room)
}

// TestClient_CompactionTrigger_DeletesAfterThreshold is #165 Task 10's test
// (a): the compaction trigger must not merely be CALLED after CompactEvery
// stored updates — Task 1's review found that calling Compact against
// LegacyAdapter's own KeepVersions=0 default is a permanent no-op, so a test
// that only checked "was Compact invoked" (or, worse, only "does load-after
// equal load-before" — TestSQLiteStore_CompactPreservesState's own doc now
// explains why that alone proves nothing) would pass identically whether
// compaction genuinely reclaims space or silently does nothing. This test
// pins KeepVersions down explicitly and checks the underlying row count,
// not just the materialized state.
//
// The assertion compares the final row count against the TOTAL number of
// StoreUpdate calls ever made, rather than against a fixed post-compaction
// row count: ygo's own server unconditionally pushes a SyncStep2 on connect
// AND replies to this client's own SyncStep1 with a second one (see
// Options.Token's doc), so every connection's handshake alone accounts for
// a couple of stored (empty, harmless, but real) rows before a single local
// edit happens — and once one compaction has fired and reset the counter,
// writes accumulate again from zero, so the exact row count left at any
// given moment depends on how many edits landed AFTER the last threshold
// crossing, not just on KeepVersions. "Fewer rows than writes ever made" is
// the assertion that is actually robust to that arithmetic while still
// proving genuine deletion happened.
func TestClient_CompactionTrigger_DeletesAfterThreshold(t *testing.T) {
	srv, ts := startServer(t)
	const room = "compact-room"

	path := filepath.Join(t.TempDir(), "compact.db")
	sqliteStore, err := OpenSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqliteStore.Close() })
	sqliteStore.KeepVersions = 3 // small and explicit, so deletion is provable quickly

	store := &countingCompactStore{SQLiteStore: sqliteStore}

	const compactEvery = 10
	c, doc := dialSynced(t, ts, room, Options{
		Store:        store,
		CompactEvery: compactEvery,
	})
	txt := doc.GetText("t")

	// Cross the threshold: more than compactEvery local edits, each round-
	// tripped through the (real) server so we know the loop's own select has
	// cycled — and therefore maybeCompact has had a chance to run — at least
	// once per edit.
	const edits = compactEvery + 4
	for i := 0; i < edits; i++ {
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
		want := i + 1
		require.Eventually(t, func() bool {
			d := srv.GetDoc(room)
			return d != nil && len([]rune(d.GetText("t").ToString())) == want
		}, 2*time.Second, 5*time.Millisecond, "edit %d never reached the server", i)
	}

	require.Eventually(t, func() bool {
		return store.compactCalls.Load() > 0
	}, 2*time.Second, 10*time.Millisecond, "Compact was never invoked after crossing CompactEvery")

	ctx := context.Background()
	rows, err := sqliteStore.Store().ListVersions(ctx, room)
	require.NoError(t, err)
	require.Less(t, int64(len(rows)), store.storeCalls.Load(),
		"stored update rows after compaction = %d, total StoreUpdate calls = %d: "+
			"Compact must have genuinely deleted some rows, not left everything in place",
		len(rows), store.storeCalls.Load())
	require.NotEmpty(t, rows, "compaction must not have deleted EVERYTHING")

	// State preserved despite compaction: what's loadable from the store
	// must match what the client's own Doc (and, transitively, the server's)
	// actually holds.
	blob, err := sqliteStore.LoadDoc(room)
	require.NoError(t, err)
	fresh := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(fresh, blob, nil))
	require.Equal(t, doc.GetText("t").ToString(), fresh.GetText("t").ToString())

	require.NoError(t, c.Close())
}

// TestClient_Close_JoinsLoopBeforeReturning is #165 Task 10's test (b), the
// "loop goroutine exited" half: Close must not return until Connect's own
// goroutine — which owns runReconnectLoop/runLoop, and is therefore the
// sole writer of this Client's socket (see loop.go's single-writer
// invariant) — has actually finished. This is checked directly rather than
// via a generous timeout: if Close returned early, connectReturned would
// simply not be closed yet by the time the non-blocking select below runs
// immediately afterward.
func TestClient_Close_JoinsLoopBeforeReturning(t *testing.T) {
	_, ts := startServer(t)
	const room = "close-join"

	c, err := New(Options{URL: wsURL(ts, room), Doc: crdt.New()})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	connectReturned := make(chan struct{})
	go func() {
		_ = c.Connect(ctx)
		close(connectReturned)
	}()

	waitSynced := statusWaiter(t, c, StateSynced)
	waitSynced()

	require.NoError(t, c.Close())

	select {
	case <-connectReturned:
	default:
		t.Fatal("Close returned before Connect's own loop goroutine exited")
	}
}

// TestClient_Close_StopsFurtherStoreWritesAndStatsStable is #165 Task 10's
// test (b), the "store writes are done" half: once Close has returned, a
// further Doc edit must never reach the store again (the observer New
// registered is unsubscribed — see Close's doc), and Stats() must remain
// safely, stably readable afterward (no panic, no data race — verified by
// -race, not by this assertion alone).
func TestClient_Close_StopsFurtherStoreWritesAndStatsStable(t *testing.T) {
	_, ts := startServer(t)
	const room = "close-store-order"

	path := filepath.Join(t.TempDir(), "close.db")
	sqliteStore, err := OpenSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqliteStore.Close() })
	store := &countingStore{LocalStore: sqliteStore}

	c, doc := dialSynced(t, ts, room, Options{Store: store})
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "before-close", nil) })

	require.Eventually(t, func() bool { return store.count() >= 1 }, 2*time.Second, 5*time.Millisecond,
		"pre-close edit never reached the store")
	beforeClose := store.count()

	require.NoError(t, c.Close())

	// The observer is unsubscribed synchronously inside Close (see its doc),
	// so this is a direct assertion, not a race against a timer: by the time
	// Close returned, no edit made from here on can ever reach onDocUpdate.
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "after-close", nil) })
	require.Equal(t, beforeClose, store.count(),
		"a doc edit made after Close returned must not reach the store")

	s1 := c.Stats()
	s2 := c.Stats()
	require.Equal(t, s1, s2, "Stats() must read stably after Close")
}

// TestClient_Close_CountsUndeliveredOutboundAsDropped is #165 Task 10's test
// (c), the central #202 invariant applied client-side: a payload Close
// could not deliver must be counted in Stats().Dropped — never silently
// discarded. See flushLane's doc addendum for the mechanism this exercises.
//
// The server here accepts the WebSocket upgrade and then never calls a
// single Read or Write method again (mirroring keepalive_test.go's dead-peer
// pattern) — so whatever this client sends sits unread, giving the client's
// own writes genuine TCP backpressure to push against. What makes the
// assertion DETERMINISTIC rather than dependent on this machine's socket
// buffer sizes is closeDrainTimeout/flushWriteTimeout being overridden to an
// ALREADY-ELAPSED deadline: net.Conn's contract guarantees a write against a
// deadline in the past fails immediately, regardless of whether the
// underlying buffer had room. That is what turns "probably blocks under
// backpressure" into "always fails, on every machine, every run."
func TestClient_Close_CountsUndeliveredOutboundAsDropped(t *testing.T) {
	withFlushWriteTimeout(t, -time.Second)
	withCloseDrainTimeout(t, -time.Second)

	upgrader := gws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	accepted := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		close(accepted)
		// Never Read, never Write — see this test's doc.
		<-release
		_ = conn.Close()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		close(release)
		ts.Close()
	})

	doc := crdt.New()
	c, err := New(Options{
		URL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/wedged",
		Doc: doc,
	})
	require.NoError(t, err)

	waitConnected := statusWaiter(t, c, StateConnected)
	connect(t, c)
	waitConnected()
	<-accepted

	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "never-delivered", nil) })

	require.NoError(t, c.Close())

	require.Positive(t, c.Stats().Dropped,
		"Close over a wedged connection must count the undelivered payload in "+
			"Stats().Dropped — never silently discard it (#202's client-side invariant)")
}

// failingStoreUpdateStore is a LocalStore whose StoreUpdate always fails, so
// a test can drive onDocUpdate's write-failure path directly, without any
// network involved.
type failingStoreUpdateStore struct{ err error }

func (s *failingStoreUpdateStore) LoadDoc(string) ([]byte, error)   { return nil, nil }
func (s *failingStoreUpdateStore) StoreUpdate(string, []byte) error { return s.err }

// TestClient_StoreWriteFailure_CountsAsDropped is #165 Task 10 finding 2's
// test: a failed local Store.StoreUpdate call must be visible, not silently
// swallowed the way it was before this task (see onDocUpdate's doc for the
// chosen remedy — log AND count — and the justification for doing both).
func TestClient_StoreWriteFailure_CountsAsDropped(t *testing.T) {
	store := &failingStoreUpdateStore{err: errors.New("disk full")}
	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: doc, Store: store})
	require.NoError(t, err)
	require.Equal(t, Stats{}, c.Stats())

	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })

	require.Equal(t, uint64(1), c.Stats().Dropped,
		"a failed local StoreUpdate must be counted, not silently swallowed")
}

// TestClient_StorePath_OpensAndOwnsStore checks Options.StorePath's
// construction-time behaviour: New opens a *SQLiteStore at the given path
// and tracks it as owned, distinct from a caller-supplied Options.Store.
func TestClient_StorePath_OpensAndOwnsStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.db")
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New(), StorePath: path})
	require.NoError(t, err)
	require.NotNil(t, c.ownedStore, "New with StorePath must open and own its own SQLiteStore")
	require.NotNil(t, c.opts.Store, "the owned store must also be wired up as this Client's LocalStore")
}

// TestNew_RejectsStoreAndStorePathTogether checks New's mutual-exclusion
// guard: with both set there is no single correct answer for which one
// Close should own, so New must reject the combination outright rather than
// silently preferring one.
func TestNew_RejectsStoreAndStorePathTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "both.db")
	store, err := OpenSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	_, err = New(Options{
		URL:       "ws://127.0.0.1:1/room",
		Doc:       crdt.New(),
		Store:     store,
		StorePath: path,
	})
	require.Error(t, err)
}

// TestClient_Close_ClosesOwnedStore checks Close's ownership rule from the
// "owns it" side: a store opened via Options.StorePath must actually be
// closed by Close. Proven directly (white-box, same package) by calling
// LoadDoc on the underlying store after Close and observing it fail — the
// only way to distinguish "Close closed it" from "Close left it open" for a
// store this test has no other handle on by design (see Options.StorePath's
// doc: the whole point is that the caller does NOT get to hold a reusable
// handle to an owned store).
func TestClient_Close_ClosesOwnedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned-close.db")
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New(), StorePath: path})
	require.NoError(t, err)
	require.NotNil(t, c.ownedStore)

	require.NoError(t, c.Close())

	_, err = c.ownedStore.LoadDoc("room")
	require.Error(t, err, "Close must have closed the owned store's underlying database handle")
}

// TestClient_Close_LeavesCallerSuppliedStoreOpen checks Close's ownership
// rule from the other side: a Store supplied directly via Options.Store —
// even one that happens to be a *SQLiteStore, i.e. the exact same
// concrete type Options.StorePath would have produced — belongs to the
// caller and must survive Close untouched, per Close's own documented
// invariant (predating this task) that this task must not weaken.
func TestClient_Close_LeavesCallerSuppliedStoreOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caller-owned.db")
	store, err := OpenSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New(), Store: store})
	require.NoError(t, err)
	require.Nil(t, c.ownedStore, "a caller-supplied Store must not be tracked as owned")

	require.NoError(t, c.Close())

	// Must still be usable: Close must not have touched it.
	require.NoError(t, store.StoreUpdate("room", testUpdate(t, "still-open")))
}

// TestClient_Close_DropsQueuedLaneContentWhenConnectNeverRan checks the
// "no loop ever ran" catch-all (dropLaneRemainder, called directly by
// Close): a Client that queued an awareness announcement before Connect was
// ever called, then was Closed without Connect ever running, must count
// that queued payload as Dropped rather than leave it silently uncounted
// forever on a lane nothing will ever drain again.
func TestClient_Close_DropsQueuedLaneContentWhenConnectNeverRan(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	require.NoError(t, err)

	// Queues onto the lane via onAwarenessUpdate without ever calling
	// Connect — see Awareness' own doc: usable the instant New returns.
	c.Awareness().SetLocalState(map[string]any{"name": "offline-only"})
	require.False(t, c.lane.Empty(), "SetLocalState before Connect must have queued onto the lane")

	require.NoError(t, c.Close())

	require.True(t, c.lane.Empty(), "Close must have drained the lane")
	require.Equal(t, uint64(1), c.Stats().Dropped,
		"a payload that never had a Connect call to ever flush it must be counted, not silently discarded")
}

// TestClient_Close_Idempotent checks Close's documented "safe to call more
// than once" contract still holds with the new join/drain/store-close
// machinery.
func TestClient_Close_Idempotent(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	require.NoError(t, err)
	require.NoError(t, c.Close())
	require.NoError(t, c.Close())
	require.NoError(t, c.Close())
}
