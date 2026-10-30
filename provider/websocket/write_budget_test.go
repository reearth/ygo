package websocket_test

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// The byte-budget tests all run over net.Pipe (via pipeListener), the same
// device TestServer_SlowPeer_GetsDisconnectedOnQueueOverflow uses: a pipe has
// no kernel buffer, so a server-side write blocks the instant the peer stops
// reading, and what a non-reading peer leaves queued is exactly what the
// server still holds for it.

// budgetServer starts a server with PeerWriteQueueBytes set, reachable only
// through pipe connections, and returns it with the dialer that reaches it.
func budgetServer(t *testing.T, budget int64, policy ygws.SlowPeerPolicy) (*ygws.Server, *gws.Dialer) {
	t.Helper()
	srv := ygws.NewServer()
	srv.PeerWriteQueueBytes = budget
	srv.SlowPeerPolicy = policy

	ln := newPipeListener()
	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return srv, pipeDialer(ln)
}

// dialPipe opens a pipe connection to room and drains the three handshake
// frames the server always sends. The returned connection is not read again
// unless the test reads it.
func dialPipe(t *testing.T, dialer *gws.Dialer, room string) *gws.Conn {
	t.Helper()
	conn, _, err := dialer.Dial("ws://pipe/"+room, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	for i := range 3 {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, err := conn.ReadMessage()
		require.NoError(t, err, "handshake frame %d", i+1)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn
}

// drainingPeer dials room and reads it until the connection ends, so the room
// keeps a peer that is never behind. The counter it returns holds how many
// frames it has read.
func drainingPeer(t *testing.T, dialer *gws.Dialer, room string) (*gws.Conn, *atomic.Int64) {
	t.Helper()
	conn := dialPipe(t, dialer, room)
	var read atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			read.Add(1)
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	return conn, &read
}

// askFullState asks the room for everything it holds — the state vector of an
// empty document, which the server answers with a full-state SyncStep2 — and
// reports the write error, so a caller flooding a connection the server is
// closing under it can stop. sendSync requires the write to succeed; here the
// failure is the point.
func askFullState(t *testing.T, conn *gws.Conn) error {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // msgSync
	enc.WriteRaw(ygsync.EncodeSyncStep1(crdt.New()))
	return conn.WriteMessage(gws.BinaryMessage, enc.Bytes())
}

// bigUpdate returns a V1 update that inserts n bytes of text as one item.
func bigUpdate(n int) []byte {
	return textUpdate(crdt.New(crdt.WithClientID(1)), strings.Repeat("x", n))
}

// step1Flood makes conn ask room for its full state up to limit times, with no
// read in between, and returns the largest queued-byte reading it saw and how
// many asks the server took before it closed the connection. It stops as soon
// as the server drops the peer.
func step1Flood(t *testing.T, srv *ygws.Server, conn *gws.Conn, room string, limit int) (peak int64, sent int) {
	t.Helper()
	for sent = 0; sent < limit; sent++ {
		if err := askFullState(t, conn); err != nil {
			return peak, sent // the server closed the connection under us
		}
		// Read the accounting, never the connection: reading would let the
		// blocked writer deliver a frame and empty the queue this measures.
		for range 20 {
			queued, peers := ygws.PeerQueuedBytes(srv, room)
			if queued > peak {
				peak = queued
			}
			if peers < 2 {
				return peak, sent + 1
			}
			time.Sleep(5 * time.Millisecond)
			if queued > 0 {
				break
			}
		}
	}
	return peak, sent
}

func TestServer_PeerWriteQueueBytes_ClosesPeerThatNeverReads(t *testing.T) {
	// A peer that sends SyncStep1 and never reads is answered a full-state
	// SyncStep2 each time, on its own write queue. The frame cap lets 512 of
	// those pile up, so the room's size times 512 is what one such connection
	// can make the server hold. With a byte budget the server closes it
	// instead, before it passes the budget by more than the one frame the
	// empty-queue rule admits.
	const (
		room     = "never-reads"
		docBytes = 512 << 10
		budget   = int64(1 << 20)
	)

	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)

	// A peer that keeps reading holds the room open, so the accounting below
	// is read from a live room rather than from one eviction has removed.
	_, reads := drainingPeer(t, dialer, room)

	conn := dialPipe(t, dialer, room)
	sendUpdate(t, conn, bigUpdate(docBytes))
	require.Eventually(t, func() bool { return reads.Load() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the reading peer should receive the update that makes the room large")

	full := crdt.EncodeStateAsUpdateV1(srv.GetDoc(room), nil)
	require.Greater(t, len(full), docBytes, "the room should now hold the large document")

	// From here conn never reads: its writer blocks on the first answer and
	// every later answer stays queued.
	peak, sent := step1Flood(t, srv, conn, room, 40)

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := conn.ReadMessage()
	require.Error(t, readErr, "a peer past its queued-byte budget must be closed by the server")
	assert.False(t, isTimeout(readErr),
		"the close must be the server's, not this read's deadline (got %v after %d SyncStep1s)", readErr, sent)

	// The one frame the empty-queue rule admits can stand above the budget;
	// nothing beyond it can.
	answer := int64(len(full)) + 64 // the full state plus its sync framing
	assert.Positive(t, peak, "the frames the peer left queued must be accounted while it holds them")
	assert.LessOrEqual(t, peak, budget+answer,
		"a peer holds at most its budget plus the one frame an empty queue admits")

	require.Eventually(t, func() bool {
		queued, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 1 && queued == 0
	}, 5*time.Second, 10*time.Millisecond,
		"the dropped peer's queued bytes must be released, leaving only the reading peer")
}

func TestServer_PeerWriteQueueBytes_ClosesOverBudgetUnderResyncPolicy(t *testing.T) {
	// SlowPeerResync keeps a peer that overflows the frame cap: the stale
	// delta is dropped and a full-state resync follows once its backlog
	// drains. The byte budget does not take that bargain — a peer over it is
	// holding memory, and its backlog drains only if it reads — so it closes
	// on this policy too.
	const (
		room     = "never-reads-resync"
		docBytes = 512 << 10
		budget   = int64(1 << 20)
	)

	srv, dialer := budgetServer(t, budget, ygws.SlowPeerResync)
	_, reads := drainingPeer(t, dialer, room)

	conn := dialPipe(t, dialer, room)
	sendUpdate(t, conn, bigUpdate(docBytes))
	require.Eventually(t, func() bool { return reads.Load() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the reading peer should receive the update that makes the room large")

	step1Flood(t, srv, conn, room, 40)

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := conn.ReadMessage()
	require.Error(t, readErr, "SlowPeerResync must not keep a peer that is over its byte budget")
	assert.False(t, isTimeout(readErr),
		"the close must be the server's, not this read's deadline (got %v)", readErr)
}

func TestServer_PeerWriteQueueBytes_ZeroKeepsTheFrameCapAlone(t *testing.T) {
	// The default, 0, is the behaviour of every release before the budget:
	// the same flood stays connected and the answers pile up on the frame
	// queue, which is what makes one connection able to exhaust a pod.
	const (
		room     = "budget-off"
		docBytes = 256 << 10
		asks     = 8
	)

	srv, dialer := budgetServer(t, 0, ygws.SlowPeerDisconnect)
	_, reads := drainingPeer(t, dialer, room)

	conn := dialPipe(t, dialer, room)
	sendUpdate(t, conn, bigUpdate(docBytes))
	require.Eventually(t, func() bool { return reads.Load() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the reading peer should receive the update that makes the room large")

	peak, sent := step1Flood(t, srv, conn, room, asks)
	require.Equal(t, asks, sent, "with no budget the server must take every SyncStep1")
	t.Logf("budget off: %d SyncStep1 frames on a %d-byte document left %d bytes queued for one peer",
		sent, docBytes, peak)

	assert.Greater(t, peak, int64(asks-2)*int64(docBytes),
		"with no budget the answers pile up: %d asks on a %d-byte document left %d bytes queued",
		sent, docBytes, peak)

	_, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 2, peers, "with no budget neither peer is closed")

	// The peer is not only still registered: its connection is live, and the
	// backlog the server built for it is delivered the moment it reads.
	outerType, _ := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType, "the queued answers are sync frames")
}

func TestServer_PeerWriteQueueBytes_ReadingPeerSurvivesABurst(t *testing.T) {
	// A peer that reads is never behind for long, so the credit the writer
	// gives back keeps it under a budget far smaller than the burst it
	// receives: 300 KiB of broadcasts through a 64 KiB budget.
	const (
		room    = "burst"
		frames  = 300
		each    = 1 << 10
		budget  = int64(64 << 10)
		pace    = time.Millisecond
		wantAll = int64(frames)
	)

	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	_, reads := drainingPeer(t, dialer, room)

	sender := dialPipe(t, dialer, room)
	doc := crdt.New(crdt.WithClientID(2))
	for range frames {
		sendUpdate(t, sender, textUpdate(doc, strings.Repeat("y", each)))
		time.Sleep(pace)
	}

	require.Eventually(t, func() bool { return reads.Load() >= wantAll }, 10*time.Second, 10*time.Millisecond,
		"a reading peer must receive the whole burst (%d of %d read)", reads.Load(), wantAll)

	queued, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 2, peers, "neither peer may be closed under a burst they keep up with")
	assert.LessOrEqual(t, queued, budget, "what is still queued stays under the budget")
}

func TestServer_PeerWriteQueueBytes_AccountingReturnsToZero(t *testing.T) {
	// The accounting is a loan, not a meter: what a peer queues is charged to
	// it and credited back, whether the frame reaches the connection or the
	// peer is dropped holding it.
	const (
		room     = "accounting"
		docBytes = 128 << 10
		budget   = int64(8 << 20) // large enough that nothing here is closed
	)

	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	_, reads := drainingPeer(t, dialer, room)

	conn := dialPipe(t, dialer, room)
	sendUpdate(t, conn, bigUpdate(docBytes))
	require.Eventually(t, func() bool { return reads.Load() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the reading peer should receive the update that makes the room large")

	// After a drain: the reading peer took the whole update, so nothing is
	// charged to either peer.
	require.Eventually(t, func() bool {
		queued, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 2 && queued == 0
	}, 5*time.Second, 10*time.Millisecond, "a drained queue leaves nothing charged")

	// After a drop: the non-reading peer leaves with frames still queued.
	peak, _ := step1Flood(t, srv, conn, room, 3)
	require.Positive(t, peak, "the non-reading peer must be holding queued bytes before it is dropped")
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool {
		queued, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 1 && queued == 0
	}, 5*time.Second, 10*time.Millisecond,
		"a dropped peer's queued bytes must be released from a room that is still open")
}
