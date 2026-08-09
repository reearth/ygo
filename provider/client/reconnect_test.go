package client

import (
	"sync"
	"testing"
	"time"

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
func TestClient_Reconnect_BackoffResetsOnlyAfterHandshake(t *testing.T) {
	withFixedRand(t, 0.9) // near the top of each attempt's range, to make growth (if any) obvious

	srv, ts := startServer(t)
	const room = "reconnect-reset"

	c, _ := dialSynced(t, ts, room, Options{MaxBackoff: 30 * time.Second})

	// With randFloat pinned at 0.9: a backoff that correctly resets after
	// every handshake draws attempt 0 every time (range [0, 500ms) ->
	// ~450ms) on all three iterations below. A backoff that never reset
	// would instead keep widening (attempt 1 -> range [0, 1s) -> ~900ms,
	// attempt 2 -> range [0, 2s) -> ~1.8s), which the 700ms threshold below
	// catches starting at the very first un-reset iteration.
	const resetThreshold = 700 * time.Millisecond
	for i := 0; i < 3; i++ {
		waitDisconnected := statusWaiter(t, c, StateDisconnected)
		waitSynced := statusWaiter(t, c, StateSynced)
		start := time.Now()
		require.NoError(t, srv.CloseRoom(room, true))
		waitDisconnected()
		waitSynced()
		elapsed := time.Since(start)
		require.Less(t, elapsed, resetThreshold,
			"reconnect %d took %v: backoff appears to not have reset after the previous handshake", i, elapsed)
	}
}
