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

// budgetServer starts a server with PeerWriteQueueBytes set, reachable only
// through pipe connections: a pipe has no kernel buffer, so a server-side
// write blocks as soon as the peer stops reading.
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

// dialPipe opens a pipe connection to room and reads the handshake.
func dialPipe(t *testing.T, dialer *gws.Dialer, room string) *gws.Conn {
	t.Helper()
	conn, _, err := dialer.Dial("ws://pipe/"+room, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	drainHandshake(t, conn, crdt.New())
	return conn
}

// drainingPeer dials room and reads it until the connection ends. The counter
// it returns holds how many frames it has read.
func drainingPeer(t *testing.T, dialer *gws.Dialer, room string) *atomic.Int64 {
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
	return &read
}

// largeRoom adds a reading peer to room, then connects conn and sends a
// docBytes-long text insert, so every SyncStep1 in room is answered with at
// least docBytes. The reading peer keeps the room open when conn is dropped.
func largeRoom(t *testing.T, dialer *gws.Dialer, room string, docBytes int) *gws.Conn {
	t.Helper()
	reads := drainingPeer(t, dialer, room)
	conn := dialPipe(t, dialer, room)
	sendUpdate(t, conn, textUpdate(crdt.New(crdt.WithClientID(1)), strings.Repeat("x", docBytes)))
	require.Eventually(t, func() bool { return reads.Load() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the reading peer should receive the update")
	return conn
}

// askFullState sends a SyncStep1 with an empty state vector, which the server
// answers with the room's full state. Unlike sendSync it returns the write
// error, because the server closing the connection is what the tests expect.
func askFullState(conn *gws.Conn) error {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // msgSync
	enc.WriteRaw(ygsync.EncodeSyncStep1(crdt.New()))
	return conn.WriteMessage(gws.BinaryMessage, enc.Bytes())
}

// step1Flood sends up to limit SyncStep1 frames on conn and never reads. It
// returns the highest queued-byte total it saw in room, and stops when the
// server closes conn.
func step1Flood(t *testing.T, srv *ygws.Server, conn *gws.Conn, room string, limit int) int64 {
	t.Helper()
	var peak int64
	for range limit {
		if err := askFullState(conn); err != nil {
			return peak
		}
		// Wait for the answer to be queued before the next ask.
		for range 20 {
			queued, peers := ygws.PeerQueuedBytes(srv, room)
			peak = max(peak, queued)
			if peers < 2 {
				return peak
			}
			time.Sleep(5 * time.Millisecond)
			if queued > 0 {
				break
			}
		}
	}
	return peak
}

// requireServerClosed reads conn and requires that the server closed it,
// not that the read timed out.
func requireServerClosed(t *testing.T, conn *gws.Conn, msg string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	require.Error(t, err, msg)
	require.False(t, isTimeout(err), "%s: the read timed out", msg)
}

func TestServer_PeerWriteQueueBytes_ClosesPeerThatNeverReads(t *testing.T) {
	const (
		room     = "never-reads"
		docBytes = 512 << 10
		budget   = int64(1 << 20)
	)
	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	conn := largeRoom(t, dialer, room, docBytes)
	full := int64(len(crdt.EncodeStateAsUpdateV1(srv.GetDoc(room), nil)))

	peak := step1Flood(t, srv, conn, room, 40)
	requireServerClosed(t, conn, "a peer past its budget must be closed")

	// An empty queue takes one frame of any size, so the peak can pass the
	// budget by one answer (the full state plus its sync framing), not more.
	assert.Positive(t, peak)
	assert.LessOrEqual(t, peak, budget+full+64)

	require.Eventually(t, func() bool {
		_, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 1
	}, 5*time.Second, 10*time.Millisecond, "the closed peer should leave the room")
}

func TestServer_PeerWriteQueueBytes_ClosesOverBudgetUnderResyncPolicy(t *testing.T) {
	const (
		room     = "never-reads-resync"
		docBytes = 512 << 10
		budget   = int64(1 << 20)
	)
	srv, dialer := budgetServer(t, budget, ygws.SlowPeerResync)
	conn := largeRoom(t, dialer, room, docBytes)

	step1Flood(t, srv, conn, room, 40)
	requireServerClosed(t, conn, "SlowPeerResync must not keep a peer past its budget")
}

func TestServer_PeerWriteQueueBytes_DeliversAFrameLargerThanTheBudget(t *testing.T) {
	const (
		room     = "oversized"
		docBytes = 256 << 10
		budget   = int64(64 << 10)
	)
	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	conn := largeRoom(t, dialer, room, docBytes)

	require.NoError(t, askFullState(conn))
	outerType, payload := readOne(t, conn, 5*time.Second)
	require.Equal(t, uint64(0), outerType)
	require.Greater(t, len(payload), docBytes, "the full state is delivered whole")

	_, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 2, peers, "the peer is not closed for a frame on an empty queue")
}

func TestServer_PeerWriteQueueBytes_ZeroKeepsTheFrameCapAlone(t *testing.T) {
	// With the default 0, the same asks stay queued and the peer stays
	// connected: all of them are delivered when it reads.
	const (
		room     = "budget-off"
		docBytes = 256 << 10
		asks     = 8
	)
	srv, dialer := budgetServer(t, 0, ygws.SlowPeerDisconnect)
	conn := largeRoom(t, dialer, room, docBytes)

	for i := range asks {
		require.NoError(t, askFullState(conn), "ask %d", i+1)
	}
	for i := range asks {
		outerType, payload := readOne(t, conn, 5*time.Second)
		require.Equal(t, uint64(0), outerType, "answer %d is a sync frame", i+1)
		require.Greater(t, len(payload), docBytes, "answer %d is the full state", i+1)
	}
	queued, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 2, peers, "with no budget neither peer is closed")
	assert.Zero(t, queued, "with no budget nothing is charged")
}

func TestServer_PeerWriteQueueBytes_ReadingPeerSurvivesABurst(t *testing.T) {
	// 300 KiB of broadcasts through a 64 KiB budget: the reading peer keeps
	// up, so its queue stays under the budget.
	const (
		room   = "burst"
		frames = 300
		each   = 1 << 10
		budget = int64(64 << 10)
	)
	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	reads := drainingPeer(t, dialer, room)

	sender := dialPipe(t, dialer, room)
	doc := crdt.New(crdt.WithClientID(2))
	for range frames {
		sendUpdate(t, sender, textUpdate(doc, strings.Repeat("y", each)))
		time.Sleep(time.Millisecond)
	}

	require.Eventually(t, func() bool { return reads.Load() >= frames }, 10*time.Second, 10*time.Millisecond,
		"the reading peer must receive the whole burst")
	queued, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 2, peers, "neither peer is closed")
	assert.LessOrEqual(t, queued, budget)
}

func TestServer_PeerWriteQueueBytes_AccountingReturnsToZero(t *testing.T) {
	const (
		room     = "accounting"
		docBytes = 128 << 10
		budget   = int64(8 << 20) // large enough that nothing here is closed
	)
	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	conn := largeRoom(t, dialer, room, docBytes)

	require.Eventually(t, func() bool {
		queued, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 2 && queued == 0
	}, 5*time.Second, 10*time.Millisecond, "a drained queue leaves nothing charged")

	peak := step1Flood(t, srv, conn, room, 3)
	require.Positive(t, peak, "the non-reading peer holds queued bytes")
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool {
		queued, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 1 && queued == 0
	}, 5*time.Second, 10*time.Millisecond, "the dropped peer leaves nothing charged in the room")
}
