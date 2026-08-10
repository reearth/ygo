package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestClient_ReconnectsAfterDisconnect_FlushesOfflineEdits is the load-
// bearing test for #165 Task 6's central claim: there is no explicit
// offline-op queue anywhere in this client. An edit made while disconnected
// is not held in a retry buffer and replayed on reconnect — it just sits in
// the Doc (and, harmlessly, in the outbound lane) until the NEXT connection's
// y-protocol handshake runs, at which point the server's SyncStep1 declares
// what it has and this client's SyncStep2 reply carries everything it is
// missing, offline edits included. If a future change ever "optimised" the
// reconnect path by skipping the handshake (e.g. sending only the lane's
// backlog instead of re-deriving the reply from the Doc's full state), this
// test is what would catch the resulting data loss.
//
// # Why srv.CloseRoom(room, true) is the forced disconnect
//
// The brief raises two more options: pinning a net.Listener under a rebuilt
// httptest.Server (stop/start without losing the port), or a toggleable
// proxy. Both work, but CloseRoom is simpler and just as deterministic: it
// force-closes every peer connection in the named room and waits for their
// disconnect handlers to finish (see inject.go's CloseRoom) before
// returning, so by the time this call returns the client's socket read is
// guaranteed to be failing — no polling or sleeping needed to know the
// disconnect has actually happened. Its side effect, evicting the room
// entirely (there is no PersistenceAdapter configured on this harness
// server), is actually a FEATURE for this test rather than a workaround:
// it proves the property under the hardest version of it. The client
// reconnects into a brand-new, empty room with no memory of what it held
// before, so EVERYTHING the server ends up with — the pre-disconnect
// content and the offline edit alike — can only have arrived via this
// client's post-reconnect SyncStep2. A test that kept server-side state
// warm across the disconnect (via persistence, or a lighter-touch kick)
// would leave open the possibility that some of what it asserts on was
// already there from before, rather than having been flushed.
//
// # Why the backoff is pinned rather than left to the real generator
//
// randFloat is fixed at a mid-range value so the reconnect's first backoff
// delay is a known, generous few hundred milliseconds (base 500ms * 0.5 =
// 250ms) rather than whatever a real jitter draw happens to produce — which,
// per Full Jitter's own contract, is legitimately anywhere in [0, 500ms)
// including values close to zero. The offline edit below is made
// synchronously, on this goroutine, immediately after observing
// StateDisconnected and before the reconnect loop's dial can possibly have
// started (that dial doesn't happen until AFTER the pinned ~250ms sleep
// elapses) — so the ordering this test depends on (edit committed to the
// Doc before the next handshake computes its SyncStep2) is guaranteed by
// the pinned delay, not by hoping the scheduler is fast enough to win an
// unbounded race.
func TestClient_ReconnectsAfterDisconnect_FlushesOfflineEdits(t *testing.T) {
	withFixedRand(t, 0.5)

	srv, ts := startServer(t)
	const room = "reconnect"

	c, doc := dialSynced(t, ts, room, Options{MaxBackoff: 2 * time.Second})
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "before-disconnect", nil) })

	require.Eventually(t, func() bool {
		d := srv.GetDoc(room)
		return d != nil && d.GetText("t").ToString() == "before-disconnect"
	}, 5*time.Second, 10*time.Millisecond, "pre-disconnect edit never reached the server")

	// Subscribe to the FULL status sequence before triggering the
	// disconnect, per statusWaiter's documented discipline: OnStatus does
	// not replay, and a fast reconnect could otherwise blow through several
	// states before a subscription made after the fact ever sees them.
	var (
		mu  sync.Mutex
		seq []State
	)
	unsub := c.OnStatus(func(s Status) {
		mu.Lock()
		seq = append(seq, s.State)
		mu.Unlock()
	})
	t.Cleanup(unsub)
	waitDisconnected := statusWaiter(t, c, StateDisconnected)

	require.NoError(t, srv.CloseRoom(room, true))
	waitDisconnected()

	// The connection is now verifiably down (CloseRoom already waited for
	// the peer's disconnect handler). This is the offline edit: it must
	// reach the server via the NEXT handshake alone, per this test's own
	// doc above.
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "offline-edit-", nil) })

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seq) >= 4 && seq[len(seq)-1] == StateSynced
	}, 5*time.Second, 10*time.Millisecond, "client never reported a full reconnect status sequence")

	mu.Lock()
	got := append([]State(nil), seq...)
	mu.Unlock()
	require.Equal(t, []State{StateDisconnected, StateConnecting, StateConnected, StateSynced}, got,
		"reconnect status sequence must be legible: Disconnected -> Connecting -> Connected -> Synced")

	require.Eventually(t, func() bool {
		d := srv.GetDoc(room)
		return d != nil && d.GetText("t").ToString() == "offline-edit-before-disconnect"
	}, 5*time.Second, 10*time.Millisecond,
		"the offline edit (and the pre-disconnect content) never reached the server's fresh room after reconnect")
}

// TestClient_Reconnect_BackoffResetsOnlyAfterHandshake protects the other
// half of backoff's contract stated in its own reset doc: a Client must not
// reset its reconnect backoff just because a dial/upgrade succeeded — only
// after a real handshake (a SyncStep2 applied) completes. This test can't
// reach into the running reconnect loop's private backoff value directly,
// so it checks the externally observable consequence instead: after a
// SECOND disconnect, the delay before the next StateConnecting must be
// governed by an attempt counter that has been reset (i.e. small, matching
// a fresh backoff's first attempt) rather than one that kept widening
// across both disconnects — which would be the case if reset were wired to
// "dial succeeded" instead of "handshake succeeded", since both of THIS
// test's disconnects DO complete a handshake before the next one.
//
// The point being protected here is really just that CloseRoom's forced
// disconnect (which happens after the handshake has already completed) is
// the exact case the "not merely a successful dial" clause in backoff's
// reset doc is about: this test proves that repeated disconnects, each one
// after a successful sync, do not make later reconnects progressively
// slower — which is what a backoff that never reset would look like.
//
// # Why the measurement is Disconnected->Connecting, not CloseRoom->Synced
//
// An earlier version of this test timed from the CloseRoom call to the
// resulting StateSynced, which bundles the backoff sleep together with the
// force-disconnect's own teardown wait, the dial, the WebSocket upgrade, the
// server re-creating the room from scratch, and the full handshake — none of
// which this assertion has any business being sensitive to, especially under
// `-race` in a package with a documented flake history (review round 1,
// Important 4). Timestamping the StateDisconnected callback and the very
// next StateConnecting callback instead isolates exactly the interval that
// IS the backoff sleep and nothing else, which is also what actually lets
// the 700ms threshold below discriminate reliably.
//
// (This still cannot, by itself, distinguish "reset on handshake" from
// "reset on dial" — every cycle here completes both, back to back. That
// finer distinction is TestClient_Reconnect_BackoffWidensWhenHandshakeNeverCompletes's
// job; see its doc.)
func TestClient_Reconnect_BackoffResetsOnlyAfterHandshake(t *testing.T) {
	withFixedRand(t, 0.9) // near the top of each attempt's range, to make growth (if any) obvious

	srv, ts := startServer(t)
	const room = "reconnect-reset"

	c, _ := dialSynced(t, ts, room, Options{MaxBackoff: 30 * time.Second})

	type event struct {
		state State
		at    time.Time
	}
	var (
		mu  sync.Mutex
		log []event
	)
	unsub := c.OnStatus(func(s Status) {
		mu.Lock()
		log = append(log, event{state: s.State, at: time.Now()})
		mu.Unlock()
	})
	t.Cleanup(unsub)

	// With randFloat pinned at 0.9: a backoff that correctly resets after
	// every handshake draws attempt 0 every time (range [0, 500ms) ->
	// ~450ms) on all three iterations below. A backoff that never reset
	// would instead keep widening (attempt 1 -> range [0, 1s) -> ~900ms,
	// attempt 2 -> range [0, 2s) -> ~1.8s), which the 700ms threshold below
	// catches starting at the very first un-reset iteration.
	const resetThreshold = 700 * time.Millisecond
	for i := 0; i < 3; i++ {
		waitSynced := statusWaiter(t, c, StateSynced)
		require.NoError(t, srv.CloseRoom(room, true))
		waitSynced()
	}

	mu.Lock()
	got := append([]event(nil), log...)
	mu.Unlock()

	// Every StateDisconnected in the reconnect loop is immediately followed
	// (with nothing else emitted in between — see runReconnectLoop) by the
	// StateConnecting that starts the next attempt. The gap between that
	// pair IS the backoff sleep.
	gaps := 0
	for i := 0; i < len(got)-1; i++ {
		if got[i].state != StateDisconnected || got[i+1].state != StateConnecting {
			continue
		}
		gaps++
		gap := got[i+1].at.Sub(got[i].at)
		require.Less(t, gap, resetThreshold,
			"backoff sleep before reconnect attempt following disconnect #%d took %v: "+
				"backoff appears to not have reset after the previous handshake", gaps, gap)
	}
	require.Equal(t, 3, gaps, "expected exactly 3 Disconnected->Connecting gaps, one per forced disconnect; got status log %+v", got)
}

// TestClient_Reconnect_BackoffWidensWhenHandshakeNeverCompletes is the test
// TestClient_Reconnect_BackoffResetsOnlyAfterHandshake cannot be (review
// round 1, Important 1): that test's every cycle completes BOTH a
// successful dial AND a successful handshake, so it cannot tell "reset on
// handshake success" apart from "reset on dial success" — an implementation
// with backoff.reset wired to the wrong event passes it identically.
//
// This test breaks that tie by constructing a bare httptest.Server (not
// ygo's own provider/websocket.Server) whose handler accepts the WebSocket
// upgrade and then immediately closes the connection, without ever reading
// or writing a single y-protocol frame. Every one of this Client's connect
// attempts therefore succeeds at the dial/upgrade and fails before any
// handshake — the exact case backoff.reset's own doc calls out as the
// reason "handshake succeeded" and not "dial succeeded" has to be the bar:
// a load balancer transiently routing to an unhealthy backend looks exactly
// like this from the client's side, and must not be treated as "connected"
// for backoff purposes.
//
// Under the correct rule, backoff never resets here (the handshake never
// completes, so onSynced is never called), so the gap between successive
// StateConnecting attempts must keep WIDENING: ~450ms, ~900ms, ~1.8s at
// randFloat pinned to 0.9. Under the mutation this test exists to catch —
// resetting on dial/upgrade success instead of handshake success — every
// attempt here would incorrectly reset, and every gap would stay pinned
// near the first attempt's ~450ms forever.
func TestClient_Reconnect_BackoffWidensWhenHandshakeNeverCompletes(t *testing.T) {
	withFixedRand(t, 0.9)

	upgrader := gws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Accept the upgrade, then abandon it immediately: no SyncStep1,
		// no SyncStep2, nothing. This is "the WebSocket connected but the
		// application-level handshake never did."
		_ = conn.Close()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c, err := New(Options{
		URL:        "ws" + strings.TrimPrefix(ts.URL, "http") + "/never-syncs",
		Doc:        crdt.New(),
		MaxBackoff: 10 * time.Second,
	})
	require.NoError(t, err)

	var (
		mu    sync.Mutex
		times []time.Time
	)
	unsub := c.OnStatus(func(s Status) {
		if s.State != StateConnecting {
			return
		}
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
	})
	t.Cleanup(unsub)

	connect(t, c)

	const wantAttempts = 4 // 1 initial attempt + 3 reconnects -> 3 measurable gaps
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(times) >= wantAttempts
	}, 10*time.Second, 10*time.Millisecond,
		"client did not reach %d StateConnecting attempts; a handshake that never "+
			"completes must still keep the reconnect loop retrying", wantAttempts)

	mu.Lock()
	got := append([]time.Time(nil), times[:wantAttempts]...)
	mu.Unlock()

	gaps := make([]time.Duration, 0, wantAttempts-1)
	for i := 1; i < len(got); i++ {
		gaps = append(gaps, got[i].Sub(got[i-1]))
	}

	require.Greater(t, gaps[1], gaps[0],
		"gap 1 (%v) must exceed gap 0 (%v): backoff should still be widening when the "+
			"handshake never completes", gaps[1], gaps[0])
	require.Greater(t, gaps[2], gaps[1],
		"gap 2 (%v) must exceed gap 1 (%v): backoff should still be widening when the "+
			"handshake never completes", gaps[2], gaps[1])
	require.Greater(t, gaps[2], 700*time.Millisecond,
		"gap 2 (attempt 2's backoff, expected ~1.8s at randFloat=0.9) was only %v: "+
			"backoff appears to have reset despite the handshake never completing", gaps[2])
}

// TestClient_Reconnect_BackoffSleepIsCtxInterruptible protects the "sleep
// the backoff while watching ctx" requirement directly (review round 1,
// Important 2): a cancelled ctx (here, via Close) during a long backoff
// sleep must return promptly, not wait out the sleep.
//
// TestClient_Close_UnblocksConnect does not prove this: it closes ~10ms
// into an attempt-0 sleep drawn from the REAL generator's [0, 500ms), and a
// bare time.Sleep would also happen to return inside that test's 5s bound
// purely because the pinned dial target (127.0.0.1:1) fails fast and the
// real jitter draw is usually nowhere near its 500ms ceiling. This test
// pins randFloat to 0.99, so the sleep it interrupts is deterministically
// close to that ceiling (~495ms), and asserts Connect returns in well under
// that — which only holds if the sleep is actually watching ctx rather than
// blocking for its full, fixed duration regardless.
func TestClient_Reconnect_BackoffSleepIsCtxInterruptible(t *testing.T) {
	withFixedRand(t, 0.99) // pins attempt 0's delay near its ceiling: ~495ms of a 500ms base

	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	require.NoError(t, err)

	disconnected := statusWaiter(t, c, StateDisconnected)
	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()
	disconnected() // the first dial has already failed; the loop is now inside its backoff sleep

	start := time.Now()
	require.NoError(t, c.Close())

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return after Close")
	}

	elapsed := time.Since(start)
	require.Less(t, elapsed, 200*time.Millisecond,
		"Connect took %v to return after Close during a pinned ~495ms backoff sleep; "+
			"the sleep does not appear to watch ctx (an uninterruptible sleep would block "+
			"for close to the full delay instead)", elapsed)
}
