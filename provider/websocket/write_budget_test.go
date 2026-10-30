package websocket_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
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

// budgetServer starts a budgeted server reachable only through pipes, which
// have no kernel buffer, so a server write blocks once the peer stops reading.
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

// drainingPeer dials room and reads it until the connection ends, counting
// the frames it reads.
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

// largeRoom makes every SyncStep1 in room answer with at least docBytes,
// keeping a reading peer so the room stays open when conn is dropped.
func largeRoom(t *testing.T, dialer *gws.Dialer, room string, docBytes int) *gws.Conn {
	t.Helper()
	reads := drainingPeer(t, dialer, room)
	conn := dialPipe(t, dialer, room)
	sendUpdate(t, conn, textUpdate(crdt.New(crdt.WithClientID(1)), strings.Repeat("x", docBytes)))
	require.Eventually(t, func() bool { return reads.Load() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the reading peer should receive the update")
	return conn
}

// askFullState asks for the room's full state and, unlike sendSync, returns
// the write error because the tests expect the server to close conn.
func askFullState(conn *gws.Conn) error {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // msgSync
	enc.WriteRaw(ygsync.EncodeSyncStep1(crdt.New()))
	return conn.WriteMessage(gws.BinaryMessage, enc.Bytes())
}

// step1Flood sends SyncStep1 frames on conn without reading until the server
// closes it, returning the highest queued-bytes total it polled for room.
func step1Flood(t *testing.T, srv *ygws.Server, conn *gws.Conn, room string, limit int) int64 {
	t.Helper()
	var peak int64
	for range limit {
		if err := askFullState(conn); err != nil {
			return peak
		}
		before := peak
		for range 20 {
			queued, peers := ygws.PeerQueuedBytes(srv, room)
			peak = max(peak, queued)
			if peers < 2 {
				return peak
			}
			if peak > before {
				break
			}
			time.Sleep(5 * time.Millisecond)
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

func TestInteg_PeerWriteQueueBytes_ClosesPeerThatNeverReads(t *testing.T) {
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

	// An empty queue takes one frame of any size, so the peak passes the
	// budget by at most one answer.
	const syncFraming = 64 // at most, around the full state in an answer
	assert.Positive(t, peak)
	assert.LessOrEqual(t, peak, budget+full+syncFraming)

	require.Eventually(t, func() bool {
		_, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 1
	}, 5*time.Second, 10*time.Millisecond, "the closed peer should leave the room")
}

// lockedBuffer is a bytes.Buffer the server's logger and the test share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestInteg_PeerWriteQueueBytes_DropsInputAfterTheClose(t *testing.T) {
	// Over TCP the server still reads buffered SyncStep1 frames after the
	// budget close, so it must refuse and log only once.
	const (
		room     = "drops-input"
		docBytes = 256 << 10
		budget   = int64(1 << 20)
	)
	var logs lockedBuffer
	srv := ygws.NewServer()
	srv.PeerWriteQueueBytes = budget
	srv.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	writer := dial(t, ts, room)
	drainHandshake(t, writer, crdt.New())
	sendUpdate(t, writer, textUpdate(crdt.New(crdt.WithClientID(1)), strings.Repeat("x", docBytes)))
	require.Eventually(t, func() bool {
		return len(crdt.EncodeStateAsUpdateV1(srv.GetDoc(room), nil)) > docBytes
	}, 5*time.Second, 10*time.Millisecond, "the room should hold the update")

	conn := dial(t, ts, room)
	for range 5000 {
		if askFullState(conn) != nil {
			break
		}
	}
	require.Eventually(t, func() bool {
		_, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 1
	}, 10*time.Second, 10*time.Millisecond, "the flooding peer should be closed")
	assert.Equal(t, 1, strings.Count(logs.String(), "peer queued bytes over budget"))
}

func TestInteg_PeerWriteQueueBytes_ClosesOverBudgetUnderResyncPolicy(t *testing.T) {
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

func TestInteg_PeerWriteQueueBytes_RoomLargerThanTheBudget(t *testing.T) {
	// A new peer asking for state before reading the handshake, as
	// y-websocket does, gets both even though each exceeds the budget.
	const (
		room     = "oversized"
		docBytes = 256 << 10
		budget   = int64(64 << 10)
	)
	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	largeRoom(t, dialer, room, docBytes)

	conn, _, err := dialer.Dial("ws://pipe/"+room, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, askFullState(conn))
	doc := crdt.New()
	drainHandshake(t, conn, doc)
	txt := doc.GetText("t")
	assert.Len(t, txt.ToString(), docBytes, "the handshake carries the whole room")

	outerType, payload := readOne(t, conn, 5*time.Second)
	require.Equal(t, uint64(0), outerType)
	require.Greater(t, len(payload), docBytes, "the answer is the full state")

	_, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 3, peers, "no peer is closed")
}

func TestInteg_PeerWriteQueueBytes_FrameQueuedBeforeTheHandshake(t *testing.T) {
	// An Apply in OnFirstPeer queues a charged frame ahead of the handshake,
	// and it must be uncharged once the peer has read everything.
	const (
		room   = "before-handshake"
		budget = int64(64 << 10)
	)
	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	insert := func(text string) {
		_ = srv.Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, text, nil) })
		})
	}
	srv.OnFirstPeer = func(context.Context, string) { insert(strings.Repeat("a", 128<<10)) }

	conn, _, err := dialer.Dial("ws://pipe/"+room, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	for range 4 { // the Apply update, then the handshake
		readOne(t, conn, 5*time.Second)
	}
	require.Eventually(t, func() bool {
		queued, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 1 && queued == 0
	}, 5*time.Second, 10*time.Millisecond, "a peer that read everything has nothing charged")

	insert("b")
	outerType, _ := readOne(t, conn, 5*time.Second)
	assert.Equal(t, uint64(0), outerType, "the next update reaches the peer")
}

func TestInteg_PeerWriteQueueBytes_LargeBroadcastKeepsAReadingPeer(t *testing.T) {
	// The reader pauses until its writer is blocked on the large frame, so the
	// test shows a frame being written is uncounted without scheduler luck.
	const (
		room   = "large-broadcast"
		small  = 20
		budget = int64(64 << 10)
	)
	srv, dialer := budgetServer(t, budget, ygws.SlowPeerDisconnect)
	reader := dialPipe(t, dialer, room)

	sender := dialPipe(t, dialer, room)
	doc := crdt.New(crdt.WithClientID(2))
	sendUpdate(t, sender, textUpdate(doc, strings.Repeat("y", 256<<10)))
	// Messages are handled in order, so this answer means the large broadcast
	// is queued for the reader.
	require.NoError(t, askFullState(sender))
	readOne(t, sender, 5*time.Second)
	require.Eventually(t, func() bool {
		queued, _ := ygws.PeerQueuedBytes(srv, room)
		return queued == 0
	}, 5*time.Second, time.Millisecond, "the reader's writer takes the large frame off the queue")

	for range small {
		sendUpdate(t, sender, textUpdate(doc, "z"))
	}
	for i := range small + 1 {
		outerType, _ := readOne(t, reader, 5*time.Second)
		require.Equal(t, uint64(0), outerType, "frame %d is a sync frame", i+1)
	}
	_, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 2, peers, "no peer is closed")
}

func TestInteg_PeerWriteQueueBytes_ZeroKeepsTheFrameCapAlone(t *testing.T) {
	// With the default 0, the same asks stay queued and are all delivered
	// when the peer reads.
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

func TestInteg_PeerWriteQueueBytes_ReadingPeerSurvivesABurst(t *testing.T) {
	// 300 KiB of broadcasts through a 64 KiB budget: the reading peer keeps
	// up, so it is not closed.
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
	}

	require.Eventually(t, func() bool { return reads.Load() >= frames }, 10*time.Second, 10*time.Millisecond,
		"the reading peer must receive the whole burst")
	_, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 2, peers, "neither peer is closed")
}

func TestInteg_PeerWriteQueueBytes_AccountingReturnsToZero(t *testing.T) {
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

	step1Flood(t, srv, conn, room, 3)
	require.Eventually(t, func() bool {
		queued, _ := ygws.PeerQueuedBytes(srv, room)
		return queued > 0
	}, 5*time.Second, 10*time.Millisecond, "the non-reading peer holds queued bytes")
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool {
		queued, peers := ygws.PeerQueuedBytes(srv, room)
		return peers == 1 && queued == 0
	}, 5*time.Second, 10*time.Millisecond, "the peer left in the room has nothing charged")
}
