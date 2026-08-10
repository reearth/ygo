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

// withCloseDrainTimeout replaces loop.go's package-level closeDrainTimeout
// var with v for the duration of the calling test, restoring the real
// default on cleanup — the same "package-level indirection purely for test
// determinism" pattern backoff_test.go's withFixedRand already establishes
// for randFloat. See closeDrainTimeout's own doc for why a test needs to
// control this independently of the handshake-path writeTimeout const.
func withCloseDrainTimeout(t *testing.T, v time.Duration) {
	t.Helper()
	orig := closeDrainTimeout
	closeDrainTimeout = v
	t.Cleanup(func() { closeDrainTimeout = orig })
}

// withCloseDrainHook installs fn as loop.go's closeDrainHook for the
// duration of the calling test, restoring the no-op default on cleanup. See
// closeDrainHook's own doc: fn runs on the loop goroutine, strictly after
// runLoop's select has already chosen its ctx.Done() case and strictly
// before that case's own flushLane(ctx, closeDrainTimeout) call — the one
// point from which a test can queue a payload and be CERTAIN it will be
// processed by that specific drain, not by the ordinary <-c.lane.Signal()
// case the same select is equally free to choose instead on any given run.
func withCloseDrainHook(t *testing.T, fn func()) {
	t.Helper()
	orig := closeDrainHook
	closeDrainHook = fn
	t.Cleanup(func() { closeDrainHook = orig })
}

// withClosePreDrainHook installs fn as client.go's closePreDrainHook for the
// duration of the calling test, restoring the no-op default on cleanup. See
// closePreDrainHook's one call site (inside Close, between unsubscribing the
// observers and running dropLaneRemainder) for what this lets a test observe
// deterministically: whether the observers are ALREADY gone by the time
// Close is about to perform its catch-all drain.
func withClosePreDrainHook(t *testing.T, fn func()) {
	t.Helper()
	orig := closePreDrainHook
	closePreDrainHook = fn
	t.Cleanup(func() { closePreDrainHook = orig })
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
	// sqliteStore.store (unexported: same package) rather than a public
	// Store() accessor — see SQLiteStore's own doc for why this package no
	// longer promotes the wrapped VersionedPersistence publicly (#165 final
	// whole-branch review, Important E).
	rows, err := sqliteStore.store.ListVersions(ctx, room)
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
// invariant) — has actually finished.
//
// # What this asserts, and why (fixed after a CI flake under -race)
//
// An earlier version of this test closed a channel from a statement placed
// immediately AFTER the `go func() { c.Connect(ctx) }()` call's own body —
// i.e. on the goroutine the test itself spawned, not on anything Connect or
// Close controls — and then checked that channel with a non-blocking
// select run from the main goroutine right after Close returned. That
// reasoning has a gap: sync.WaitGroup.Done (which Connect calls via defer,
// and which is exactly what Close's connectWG.Wait() blocks on — see
// Close's doc, step 1) unblocks the WAITER the instant the count reaches
// zero; it does not wait for the goroutine that called Done to take even
// one more step. So there is a real window, between Connect's defer firing
// and that same goroutine being rescheduled to reach the next statement,
// during which the main goroutine can legitimately finish Close() and run
// the following select before the spawned goroutine's own close() has
// executed. That is scheduling latency on a goroutine the test doesn't
// control, not a broken invariant — and it is exactly the kind of gap the
// race detector's altered scheduling is prone to widening enough to hit,
// which is what made this flake CI-visible on both Go 1.23 and 1.26 while
// passing reliably without -race.
//
// The fix is to assert on a signal Connect's own goroutine produces WHILE
// still inside the window connectWG.Wait() blocks on, rather than on a
// further statement that only runs AFTER Connect has already returned to
// its caller. Connect's own doc establishes exactly such a signal: the
// clean StateDisconnected{Err: nil} bookend it emits via c.emitStatus is
// called strictly before Connect returns and therefore strictly before its
// deferred connectWG.Done() fires (see Connect's doc, and the ordering
// finding — final whole-branch review, Important A — that pinned this down
// after a prior bug let a similar emission slip outside that window). A
// subscriber registered before Connect starts (so it cannot miss the
// emission — OnStatus does not replay, see statusWaiter's doc for the same
// concern) is therefore guaranteed to have already observed that status by
// the time Close's connectWG.Wait() can possibly return, which is itself a
// precondition for Close returning at all (Close's doc, step 1). Observing
// it after Close returns is production-controlled proof that Connect's own
// goroutine had already finished running — not a race against unrelated
// scheduling.
func TestClient_Close_JoinsLoopBeforeReturning(t *testing.T) {
	_, ts := startServer(t)
	const room = "close-join"

	c, err := New(Options{URL: wsURL(ts, room), Doc: crdt.New()})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Subscribed BEFORE Connect is ever started, so it cannot miss the
	// clean bookend emission described above, however early it happens to
	// land relative to this goroutine being scheduled.
	var cleanDisconnect atomic.Bool
	unsub := c.OnStatus(func(s Status) {
		if s.State == StateDisconnected && s.Err == nil {
			cleanDisconnect.Store(true)
		}
	})
	defer unsub()

	go func() { _ = c.Connect(ctx) }()

	waitSynced := statusWaiter(t, c, StateSynced)
	waitSynced()

	require.NoError(t, c.Close())

	require.True(t, cleanDisconnect.Load(),
		"Close returned before Connect's clean StateDisconnected bookend was observed — Connect's own "+
			"goroutine (and therefore runReconnectLoop/runLoop, and the read pump it joins) cannot yet "+
			"have finished running")
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

// TestClient_Close_UnsubscribesObserversBeforeDrainingLane is #165 Task 10
// review's Important 2: Close must unsubscribe the Doc/Awareness observers
// BEFORE running its catch-all lane drain (dropLaneRemainder), not after.
//
// The bug this guards against: nothing stops an application goroutine from
// calling doc.Transact (or Awareness().SetLocalState) concurrently with
// Close tearing down. Under the OLD order (drain, then unsubscribe), such a
// call could land a payload on the lane via the still-live observer STRICTLY
// AFTER the one and only drain Close ever performs — leaving it permanently
// stuck, uncounted, and silently unreachable once Close returns. Swapping
// the order closes this structurally: by the time the drain runs, the
// observer is already gone, so nothing new can land on the lane through it
// from that point on.
//
// withClosePreDrainHook fires at the exact point between the two steps (see
// closePreDrainHook's one call site inside Close) and simulates the race
// directly, by calling doc.Transact from inside the hook, rather than trying
// to win a real goroutine race on a schedule this test does not control.
// Under the fixed order, the observer is already unsubscribed by the time
// the hook runs, so this Transact call must be a complete no-op for the
// store — proving unsub happens before the drain, not merely usually.
//
// # Mutation-derived RED
//
// This ordering bug has no OTHER way to produce a naturally-failing test:
// reproducing it for real needs an actual concurrent race with an
// unpredictable, machine-dependent window, and a test that depended on
// winning that race would be exactly the kind of flake this package already
// has a documented history of (see the review that prompted this test).
// Verified instead by temporarily reverting Close's step order (moving
// unsubObserver/unsubAwareness back to AFTER dropLaneRemainder+
// closePreDrainHook, matching the pre-review code) and re-running this
// test: it failed, because the hook's doc.Transact call reached the
// still-live observer and wrote an extra row to the store before
// dropLaneRemainder ran, and the assertion below caught it. See this task's
// report for the exact command and captured output; the mutation was
// reverted immediately afterward and is not part of this diff.
func TestClient_Close_UnsubscribesObserversBeforeDrainingLane(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.db")
	sqliteStore, err := OpenSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqliteStore.Close() })
	store := &countingStore{LocalStore: sqliteStore}

	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: doc, Store: store})
	require.NoError(t, err)

	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "before-close", nil) })
	beforeClose := store.count()
	require.Equal(t, uint64(1), beforeClose)

	withClosePreDrainHook(t, func() {
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "raced-during-close", nil) })
	})

	require.NoError(t, c.Close())

	require.Equal(t, beforeClose, store.count(),
		"a Transact call occurring at the point Close is about to run its catch-all lane drain must not "+
			"reach the store — the observer must already be unsubscribed by then (#165 Task 10 review, "+
			"Important 2)")
	require.True(t, c.lane.Empty(), "Close must still end with an empty lane")
}

// TestClient_Close_CountsUndeliveredOutboundAsDropped is #165 Task 10's test
// (c), the central #202 invariant applied client-side: a payload Close could
// not deliver must be counted in Stats().Dropped — never silently discarded.
// See Client.countUndeliverable's doc for the exact accounting rule this
// exercises: a KindSync payload with no Store configured, which is what this
// test uses, is always counted once it fails to reach the wire.
//
// The server here accepts the WebSocket upgrade and then never calls a
// single Read or Write method again (mirroring keepalive_test.go's dead-peer
// pattern) — so whatever this client sends sits unread, giving the client's
// own writes genuine TCP backpressure to push against. closeDrainTimeout is
// overridden to an ALREADY-ELAPSED deadline so the WRITE failure itself is
// deterministic: net.Conn's contract guarantees a write against a deadline
// in the past fails immediately, regardless of whether the underlying
// buffer had room — "probably blocks under backpressure" becomes "always
// fails, on every machine, every run."
//
// # A deterministic write failure is not the same as a deterministic test (#165 Task 10 review, Minor 4)
//
// An earlier version of this test queued its payload BEFORE calling Close,
// then called Close and asserted Dropped afterward. That left the queued
// payload available to EITHER of runLoop's select cases — its ordinary
// <-c.lane.Signal() case, or (once Close's teardown reached the loop) its
// <-ctx.Done() case — and Go does not guarantee which ready case a select
// picks. Removing the isClosing() check that used to accompany that version
// (see git history) made the choice of branch matter for correctness, and
// it flaked (Dropped read 0) roughly 1 run in 100–300 under -count as a
// result. Client.countUndeliverable's rule now decides purely from (payload
// kind, Options.Store), so which branch happens to process this specific
// failure no longer changes the outcome for THIS scenario (Store is nil
// here, so either path counts it) — but depending on that being true, rather
// than pinning it down, is exactly the kind of incidental correctness a
// later, unrelated change to the counting rule could quietly break again
// without this test ever noticing. withCloseDrainHook below removes the race
// outright instead of merely making it harmless: it queues the payload from
// INSIDE the ctx.Done() case's own hook, a point reachable only after that
// case has already been selected — so this test exercises Close's own
// bounded drain specifically, every run, rather than "whichever of two
// paths happened to win."
func TestClient_Close_CountsUndeliveredOutboundAsDropped(t *testing.T) {
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
	withCloseDrainHook(t, func() {
		// See this test's doc: fires strictly after runLoop's ctx.Done()
		// case has already been selected, so this payload is guaranteed to
		// be processed by Close's own bounded drain, not the ordinary
		// <-c.lane.Signal() case.
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "never-delivered", nil) })
	})

	require.NoError(t, c.Close())

	require.Positive(t, c.Stats().Dropped,
		"Close over a wedged connection must count the undelivered payload in "+
			"Stats().Dropped — never silently discard it (#202's client-side invariant)")
}

// TestClient_Close_DoesNotCountStoreBackedSyncEvenWhenUndelivered is #165
// Task 10 review's Important 3, exercised through the SAME wedged-connection
// close-time drain as the test immediately above, but with a Store
// configured this time: per Client.countUndeliverable's rule, a KindSync
// payload is NEVER counted as Dropped when Options.Store is set, no matter
// when in this Client's lifecycle it fails to reach the wire, including
// inside Close's own drain — the Store already durably holds it (onDocUpdate
// wrote it there, synchronously, before it was ever queued), so the next
// hydrate+handshake delivers it regardless of whether THIS connection ever
// does. This is the offline-edit-then-close scenario the package's own doc
// centers around, made concrete: Dropped must read zero, and the edit must
// actually be sitting in the Store, proving nothing was really lost despite
// the network drain failing exactly as it does in the sibling test above.
func TestClient_Close_DoesNotCountStoreBackedSyncEvenWhenUndelivered(t *testing.T) {
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
		<-release
		_ = conn.Close()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		close(release)
		ts.Close()
	})

	const room = "wedged-store-room"
	path := filepath.Join(t.TempDir(), "wedged-store.db")
	store, err := OpenSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	doc := crdt.New()
	c, err := New(Options{
		URL:   "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + room,
		Doc:   doc,
		Store: store,
	})
	require.NoError(t, err)

	waitConnected := statusWaiter(t, c, StateConnected)
	connect(t, c)
	waitConnected()
	<-accepted

	txt := doc.GetText("t")
	withCloseDrainHook(t, func() {
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "durable-but-undelivered", nil) })
	})

	require.NoError(t, c.Close())

	require.Zero(t, c.Stats().Dropped,
		"a KindSync payload with a Store configured must never be counted as Dropped, even when "+
			"Close's own network drain could not deliver it — the Store already has it durably "+
			"(#165 Task 10 review, Important 3)")

	blob, err := store.LoadDoc(room)
	require.NoError(t, err)
	require.NotEmpty(t, blob, "the edit must actually be durable in the Store despite never reaching the wire")
	fresh := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(fresh, blob, nil))
	require.Equal(t, "durable-but-undelivered", fresh.GetText("t").ToString())
}

// TestFlushLane_OrdinaryFailureCountsStorelessSyncImmediately is #165 Task
// 10 review's Important 1: a KindSync payload with no Store configured must
// be counted as Dropped the INSTANT its write fails — even while ctx is
// still live, i.e. even on the ordinary in-session path, before Close has
// been called at all and before runReconnectLoop has any chance to retry.
//
// This is a genuinely different scenario from the two Close-time tests
// above: there, ctx is already done by the time the write is attempted.
// Here it deliberately is NOT — context.Background() never cancels —
// mirroring the review's own reported scenario verbatim: "the connection
// drops mid-flush ... ctx.Err()==nil ... the payload — already taken off
// the lane — is discarded and an error returned; runReconnectLoop backs
// off; the app calls Close() during that backoff; dropLaneRemainder finds
// an empty lane; Dropped reads 0 with the payload never delivered." Deciding
// whether to count purely from (payload kind, Options.Store) — see
// Client.countUndeliverable — rather than from ctx state closes that gap
// structurally: the decision is never deferred to begin with, so there is
// no later point at which it could still turn out wrong.
//
// This calls session.flushLane directly against a real (but never-reading)
// WebSocket server, bypassing runLoop's select loop entirely — deterministic
// by construction, with no goroutine-scheduling race of any kind, unlike a
// test that had to arrange for a real Close to race a real backoff window.
func TestFlushLane_OrdinaryFailureCountsStorelessSyncImmediately(t *testing.T) {
	upgrader := gws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		<-release
		_ = conn.Close()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		close(release)
		ts.Close()
	})

	dialer := gws.Dialer{}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/room", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: doc}) // no Store; never dialed
	require.NoError(t, err)

	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "mid-session-loss", nil) })
	require.False(t, c.lane.Empty(), "the local edit must have queued onto the lane")

	s := &session{c: c, conn: conn}
	ctx := context.Background()          // deliberately live: ctx.Err() == nil throughout
	err = s.flushLane(ctx, -time.Second) // already-elapsed deadline: the write fails immediately
	require.Error(t, err, "flushLane must still propagate the write failure so runLoop tears down and reconnects")

	require.Equal(t, uint64(1), c.Stats().Dropped,
		"a KindSync payload with no Store configured must be counted the instant its write fails, even "+
			"with ctx still live — waiting to see whether a future reconnect happens to arrive before Close "+
			"is called is exactly the reasoning #165 Task 10's review found broken (Important 1)")
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

// TestClient_Close_DropsQueuedAwarenessWhenConnectNeverRan checks the "no
// loop ever ran" catch-all (dropLaneRemainder, called directly by Close): a
// Client that queued an awareness announcement before Connect was ever
// called, then was Closed without Connect ever running, must count that
// queued payload as Dropped rather than leave it silently uncounted forever
// on a lane nothing will ever drain again. Per Client.countUndeliverable's
// rule, a KindAwareness payload is counted UNCONDITIONALLY — Store or no
// Store — since there is no store equivalent for awareness state; see the
// two sibling tests below for the KindSync side of the same rule, where
// Store's presence changes the outcome.
func TestClient_Close_DropsQueuedAwarenessWhenConnectNeverRan(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	require.NoError(t, err)

	// Queues onto the lane via onAwarenessUpdate without ever calling
	// Connect — see Awareness' own doc: usable the instant New returns.
	c.Awareness().SetLocalState(map[string]any{"name": "offline-only"})
	require.False(t, c.lane.Empty(), "SetLocalState before Connect must have queued onto the lane")

	require.NoError(t, c.Close())

	require.True(t, c.lane.Empty(), "Close must have drained the lane")
	require.Equal(t, uint64(1), c.Stats().Dropped,
		"a queued awareness payload that never had a Connect call to ever flush it must be counted, "+
			"not silently discarded — awareness has no durable backstop the way a Store-backed KindSync "+
			"payload does")
}

// TestClient_Close_DoesNotDropQueuedSyncWhenStoreConfigured is #165 Task 10
// review's Important 3, exercised through dropLaneRemainder directly (no
// network, no live loop — Connect is never called) rather than through
// flushLane's close-time drain (see
// TestClient_Close_DoesNotCountStoreBackedSyncEvenWhenUndelivered for that
// path): a KindSync payload queued by a local Doc edit, WITH a Store
// configured, must NOT be counted as Dropped when Close drains it unsent —
// the Store already durably holds the edit (onDocUpdate wrote it there
// before ever queueing it), and the package's own central design claim is
// that the next hydrate+handshake delivers it from there. This is precisely
// the scenario the package doc is built around: an app edits its Doc
// offline and closes without ever having synced at all.
func TestClient_Close_DoesNotDropQueuedSyncWhenStoreConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-connected.db")
	store, err := OpenSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: doc, Store: store})
	require.NoError(t, err)

	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "offline-only-edit", nil) })
	require.False(t, c.lane.Empty(), "the local edit must have queued onto the lane")

	require.NoError(t, c.Close())

	require.True(t, c.lane.Empty(), "Close must still have drained the lane")
	require.Zero(t, c.Stats().Dropped,
		"a queued KindSync payload with a Store configured must not be counted, even when Connect "+
			"was never called to ever flush it — the Store already has it durably")

	blob, err := store.LoadDoc("room")
	require.NoError(t, err)
	require.NotEmpty(t, blob, "the edit must be durable in the Store despite never being flushed")
}

// TestClient_Close_DropsQueuedSyncWhenNoStoreConfigured is
// TestClient_Close_DoesNotDropQueuedSyncWhenStoreConfigured's mirror image:
// the SAME scenario (a local edit queued, Connect never called, Close drains
// the lane) but with NO Store configured. Per Client.countUndeliverable's
// rule, a KindSync payload IS counted when there is no Store: nothing
// durable backs it, and — Connect having never run at all — this Client
// never even gets an in-memory reconnect chance to resend it, let alone a
// durable one.
func TestClient_Close_DropsQueuedSyncWhenNoStoreConfigured(t *testing.T) {
	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: doc})
	require.NoError(t, err)

	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "lost-offline-edit", nil) })
	require.False(t, c.lane.Empty(), "the local edit must have queued onto the lane")

	require.NoError(t, c.Close())

	require.True(t, c.lane.Empty(), "Close must have drained the lane")
	require.Equal(t, uint64(1), c.Stats().Dropped,
		"a queued KindSync payload with NO Store configured must be counted when nothing ever flushes it")
}

// failingLoadDocStore is a LocalStore whose LoadDoc always fails, so a test
// can drive Connect's hydrate-failure branch deterministically, without any
// network or real corruption involved.
type failingLoadDocStore struct{ err error }

func (s *failingLoadDocStore) LoadDoc(string) ([]byte, error)   { return nil, s.err }
func (s *failingLoadDocStore) StoreUpdate(string, []byte) error { return nil }

// TestClient_Connect_HydrateFailureKeepsCloseWaitingForStatus is the final
// whole-branch review's Important A: Connect's hydrate-failure branch used
// to reset connectStarted to false BEFORE calling emitStatus, so a Close
// running concurrently on another goroutine could read connectStarted as
// already false — under connectMu, the same lock Connect's own reset takes —
// and skip connectWG.Wait() entirely. That let Close return while the
// hydrate-failure OnStatus callback was still running, which is exactly the
// guarantee Connect's own doc (and #165 Task 11's review, which this
// mirrors for the ordinary path) claims: "before Close's connectWG.Wait()
// can possibly return, exactly like every other status this Client ever
// emits."
//
// This drives the race deterministically rather than depending on winning
// it: an OnStatus callback for the hydrate-failure status blocks on a
// channel the test controls, so the callback's in-flight window is exactly
// as long as the test wants it to be. A Close spawned while that callback
// is deliberately still blocked must not return until the test releases it.
func TestClient_Connect_HydrateFailureKeepsCloseWaitingForStatus(t *testing.T) {
	store := &failingLoadDocStore{err: errors.New("simulated disk read failure")}
	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: doc, Store: store})
	require.NoError(t, err)

	callbackStarted := make(chan struct{})
	release := make(chan struct{})
	var calledOnce atomic.Bool
	c.OnStatus(func(st Status) {
		if st.State == StateDisconnected && st.Err != nil && calledOnce.CompareAndSwap(false, true) {
			close(callbackStarted)
			<-release // held open until the test says otherwise
		}
	})

	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(context.Background()) }()

	select {
	case <-callbackStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("hydrate-failure OnStatus callback never started")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()

	// The callback is deliberately still blocked in <-release right now.
	// Close must not have returned yet — give it a generous window to
	// (incorrectly) do so before declaring the invariant held.
	select {
	case <-closeDone:
		t.Fatal("Close returned while the hydrate-failure OnStatus callback was still running — " +
			"connectStarted must not be released before emitStatus completes (#165 final review, Important A)")
	case <-time.After(200 * time.Millisecond):
	}

	close(release) // let the callback finish

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close never returned after the hydrate-failure callback finished")
	}
	<-connectDone
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
