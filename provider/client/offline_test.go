package client

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// Settle-detection tuning for TestClient_SuppressesEchoAfterConvergence.
// See that test's doc for why this is poll-based rather than a fixed sleep.
const (
	echoSettlePollInterval = 50 * time.Millisecond
	echoSettleStableRuns   = 3
	echoSettleTimeout      = 5 * time.Second
	echoQuietWindow        = 500 * time.Millisecond
)

// waitForCountToSettle polls count every echoSettlePollInterval and returns
// once it has read the SAME value echoSettleStableRuns times in a row (the
// wire is presumed quiescent), or (0, false) if echoSettleTimeout elapses
// first without ever seeing that many identical readings in a row — which is
// what an echo storm looks like: the count never stops changing.
func waitForCountToSettle(count func() uint64) (settled uint64, ok bool) {
	deadline := time.Now().Add(echoSettleTimeout)
	var last uint64
	streak := 0
	for {
		cur := count()
		if streak > 0 && cur == last {
			streak++
		} else {
			streak = 1
		}
		last = cur
		if streak >= echoSettleStableRuns {
			return cur, true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(echoSettlePollInterval)
	}
}

// TestClient_SuppressesEchoAfterConvergence protects the "send?" half of
// onDocUpdate's origin table (#165): a remote-origin update must be stored
// but never re-sent. Without that check, two clients relaying through the
// same server bounce every update back and forth forever: B echoes what it
// received from A, the server relays that to A, A (wrongly) echoes it back
// too, and so on. The loop does not need NEW content to keep running —
// crdt.Doc.Transact fires OnUpdate for every COMMITTED transaction, even one
// that integrates an already-known update and changes nothing (see
// buildPhase2 in crdt/doc.go, which fires unconditionally whenever there is
// at least one OnUpdate subscriber) — so a redundant echo still produces a
// callback to bounce again.
//
// This package cannot observe wire frames directly, so the proxy used here is
// calls into client A's Store: the "store everything" gate elsewhere in this
// file establishes that EVERY update ever applied to a Doc — local or
// remote — reaches StoreUpdate first. That makes the store-call count a
// faithful stand-in for "how many updates has this Doc's observer processed
// so far", which is exactly what a ping-pong would inflate. A converged,
// echo-suppressing system makes one store call for A's own insert (plus
// whatever the initial handshake needed) and then falls silent; a
// ping-ponging one keeps calling it for as long as the quiet window lasts,
// as fast as the loopback socket and goroutine scheduler allow.
//
// Settling is detected by POLLING store.count(), not by sleeping a fixed
// buffer and hoping it was long enough: a fixed buffer has to guess how long
// the legitimate A -> server -> B round trip (plus any handshake tail) takes
// on whatever machine is running the test, and a loaded CI runner can make
// that guess wrong — producing a failure that has nothing to do with an echo
// regression. waitForCountToSettle instead waits for N consecutive identical
// readings, which adapts to however long real convergence actually takes,
// while still failing loudly on its own timeout if the count never stops
// changing (exactly what a genuine echo storm looks like — see the mutation
// evidence in the task report). Only the final regression check — does the
// count grow across a further quiet window once already settled — uses a
// plain sleep, because that duration is not being used to infer "settled";
// it only needs to be long enough for an echo storm to show itself, and
// running it longer than necessary on a slow machine costs time, not
// correctness.
func TestClient_SuppressesEchoAfterConvergence(t *testing.T) {
	_, ts := startServer(t)
	const room = "echo"

	dbPath := filepath.Join(t.TempDir(), "echo.db")
	sqlite, err := OpenSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlite.Close() })
	store := &countingStore{LocalStore: sqlite}

	_, docA := dialSynced(t, ts, room, Options{Store: store})
	_, docB := dialSynced(t, ts, room, Options{})

	txt := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hello", nil) })

	require.Eventually(t, func() bool {
		return docB.GetText("t").ToString() == "hello"
	}, 5*time.Second, 10*time.Millisecond, "A's insert never reached B")

	settled, ok := waitForCountToSettle(store.count)
	require.True(t, ok,
		"store-update count never settled (never saw %d consecutive identical "+
			"readings within %s); a count that never stops changing means updates "+
			"are ping-ponging instead of converging", echoSettleStableRuns, echoSettleTimeout)

	time.Sleep(echoQuietWindow)
	require.Equal(t, settled, store.count(),
		"store-update count grew during a %s quiet window after settling; "+
			"updates are ping-ponging instead of settling", echoQuietWindow)
}

// TestClient_RestartHydratesServerAuthoredTextOffline is the end-to-end form
// of the "store everything" gate (#165). TestClient_PersistsUpdatesReceivedFromServer
// already proves the isolated half of this — that a server-received update
// reaches the store — but that is not the same as proving the property that
// actually matters to an embedder: that a SECOND, unrelated Client process,
// pointed at the same on-disk store after the first one is gone and the
// server is unreachable, ends up with that content anyway. Skipping
// persistence for remote-origin updates would pass every test that only
// inspects the live Doc (it holds the server's content right up until the
// process exits) and only show up here, on the next cold start.
//
// Client A never edits anything itself: everything in its Doc when it closes
// came from the server. Client B is a fresh Client, fresh Doc, and a fresh
// SQLiteStore handle opened at the SAME path, dialing a server that has
// already been shut down (ts.Close(), called before B is even constructed).
// If B's Doc has the text, it can only have gotten there from disk.
func TestClient_RestartHydratesServerAuthoredTextOffline(t *testing.T) {
	srv, ts := startServer(t)
	const room = "restart-hydrate"

	require.NoError(t, srv.Apply(context.Background(), room,
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			// GetText outside transact: taking it inside would deadlock on
			// the doc lock Transact already holds.
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "server-authored", nil) })
		}))

	dbPath := filepath.Join(t.TempDir(), "restart.db")
	storeA, err := OpenSQLiteStore(dbPath)
	require.NoError(t, err)

	docA := crdt.New()
	a, err := New(Options{URL: wsURL(ts, room), Doc: docA, Store: storeA})
	require.NoError(t, err)

	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan error, 1)
	go func() { doneA <- a.Connect(ctxA) }()
	select {
	case <-a.Synced():
	case <-time.After(5 * time.Second):
		t.Fatal("client A did not sync within 5s")
	}
	require.Equal(t, "server-authored", docA.GetText("t").ToString(),
		"client A must have received the server-authored content before going offline")

	// Tear client A down completely, including its store handle, and wait for
	// Connect to actually return before touching the database again — exactly
	// what a real process exit guarantees and a mid-flight close would not.
	cancelA()
	require.NoError(t, a.Close())
	select {
	case <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("client A's Connect did not return after cancel + Close")
	}
	require.NoError(t, storeA.Close())

	// Stop the server before client B is even constructed: nothing below is
	// allowed to depend on a live network at all.
	ts.Close()

	storeB, err := OpenSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = storeB.Close() })

	docB := crdt.New()
	b, err := New(Options{URL: wsURL(ts, room), Doc: docB, Store: storeB})
	require.NoError(t, err)

	// Armed before connect (see statusWaiter's doc): StateConnecting is
	// reported at the top of runLoop, strictly after Connect's synchronous
	// hydrate() call has already returned, so observing it proves hydration
	// has already run — with no live connection anywhere in the picture.
	wait := statusWaiter(t, b, StateConnecting)
	connect(t, b)
	wait()

	require.Equal(t, "server-authored", docB.GetText("t").ToString(),
		"a fresh client at the same store path, with no server reachable, must hydrate "+
			"the server-authored content from disk alone")
}
