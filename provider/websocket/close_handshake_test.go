package websocket_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// The close the server sends in these tests: for every update a peer sends, or
// for an embedder's cancel cause.
const (
	refusalCode   = 4413
	refusalReason = "too large"
)

// storedDoc is a PersistenceAdapter whose every room loads the same stored
// state.
type storedDoc []byte

func (d storedDoc) LoadDoc(string) ([]byte, error) { return d, nil }
func (storedDoc) StoreUpdate(string, []byte) error { return nil }

// refusingServer serves rooms that load stored and refuse every update a peer
// sends with a close of refusalCode and refusalReason. configure, when
// non-nil, runs before the server starts.
func refusingServer(t *testing.T, stored []byte, configure func(*ygws.Server)) *httptest.Server {
	t.Helper()
	srv := ygws.NewServerWithPersistence(storedDoc(stored))
	srv.OnPeerUpdate = func(ygws.PeerUpdateInfo, func() error) error {
		return &ygws.PeerClose{Code: refusalCode, Reason: refusalReason}
	}
	if configure != nil {
		configure(srv)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { shutdownServer(srv) })
	return ts
}

// shutdownServer shuts srv down and waits for its connections' goroutines, so
// what a test's rooms still do once it ends - a closing handshake's wait, a
// room's teardown - is over before the next test runs, and competes with it
// for neither the CPUs nor the heap it measures.
func shutdownServer(srv *ygws.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), closeReadWait)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// closeReadWait is how long a test waits for what the server sends a peer it
// closes: far longer than a busy machine takes to deliver it, and still a
// bound, so a close frame that never comes fails the test.
const closeReadWait = 30 * time.Second

// causeServer serves rooms that load stored, and hands the test the cancel of
// each connection's request context, as an embedder revoking a session holds
// it.
func causeServer(t *testing.T, stored []byte) (*httptest.Server, <-chan context.CancelCauseFunc) {
	t.Helper()
	srv := ygws.NewServerWithPersistence(storedDoc(stored))
	cancels := make(chan context.CancelCauseFunc, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		cancels <- cancel
		srv.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { shutdownServer(srv) })
	return ts, cancels
}

// textState is a stored state holding n bytes of text.
func textState(n int) []byte {
	stored := crdt.New(crdt.WithClientID(1))
	text := stored.GetText("t")
	stored.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, strings.Repeat("x", n), nil) })
	return crdt.EncodeStateAsUpdateV1(stored, nil)
}

// serverLog is a log destination that the server's goroutines write while a
// test reads it.
type serverLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *serverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *serverLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// readClose reads conn until it fails, and requires the failure to be a close
// frame carrying refusalCode.
func readClose(t *testing.T, conn *gws.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(closeReadWait)))
	var err error
	for err == nil {
		_, _, err = conn.ReadMessage()
	}
	var closed *gws.CloseError
	require.ErrorAs(t, err, &closed, "the peer must receive the close frame")
	require.Equal(t, refusalCode, closed.Code)
}

// dialSmallReceiveBuffer connects to room with the client socket's receive
// buffer cut to a few KiB, so that what the server writes while the client
// does not read waits in the server's send buffer. The buffer is cut before
// the socket connects, as a client's own buffer is sized. Linux sets the
// window a socket offers from its buffer when it connects, and the server
// sizes its segments to half the largest window it has been offered; a buffer
// cut after connecting holds less than one such segment. Linux then sends the
// server's data only as zero-window probes, whose timer restarts on every
// acknowledgement the client sends, so a client that keeps sending, as these
// tests' clients do, receives nothing more however long the server waits.
func dialSmallReceiveBuffer(t *testing.T, ts *httptest.Server, room string) *gws.Conn {
	t.Helper()
	dialer := gws.Dialer{NetDialContext: (&net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		if cerr := c.Control(func(fd uintptr) { err = setReceiveBuffer(fd, 4<<10) }); cerr != nil {
			return cerr
		}
		return err
	}}).DialContext}
	conn, _, err := dialer.Dial(wsURL(ts, room), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// requireCloseWhileSending requires conn to receive the close frame, with its
// code and reason, while the peer acts as a busy browser does: it reads nothing
// for 200 ms, so that what the server writes meanwhile, the close among it,
// waits in the buffers at both ends, and it keeps sending awareness all the
// while (requireCloseWhileSendingAfter).
func requireCloseWhileSending(t *testing.T, conn *gws.Conn) {
	t.Helper()
	requireCloseWhileSendingAfter(t, conn, 200*time.Millisecond)
}

// requireCloseWhileSendingAfter requires conn to receive the close frame, with
// its code and reason, while the peer reads nothing for pause and sends
// awareness all the while, until it has read the frame. A server that closes
// the socket while the peer still sends receives that awareness on a closed
// socket, which resets the connection and discards what the peer had not yet
// received, the close frame among it: the peer then reads a reset and no code.
// The peer's awareness must not fail but for its own close in answer, so a
// reset is seen also where the stack still hands the peer what it had
// received.
func requireCloseWhileSendingAfter(t *testing.T, conn *gws.Conn, pause time.Duration) {
	t.Helper()
	presence := awareness.New(2)
	presence.SetLocalState(map[string]any{"cursor": 7})
	frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1) // msgAwareness
		enc.WriteVarBytes(presence.EncodeUpdate(nil))
	})
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(pause+closeReadWait)))
	stop, sending := make(chan struct{}), make(chan struct{})
	var sendErr error
	go func() {
		defer close(sending)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			// Fails once the client has answered the close with its own, or
			// once the connection is gone.
			if err := conn.WriteMessage(gws.BinaryMessage, frame); err != nil {
				sendErr = err
				return
			}
		}
	}()

	// The server writes what the buffers take, then the close.
	time.Sleep(pause)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(closeReadWait)))
	var err error
	for err == nil {
		_, _, err = conn.ReadMessage()
	}
	close(stop)
	<-sending

	var closed *gws.CloseError
	require.ErrorAs(t, err, &closed, "the peer must receive the close frame")
	assert.Equal(t, refusalCode, closed.Code)
	assert.Equal(t, refusalReason, closed.Text)
	if sendErr != nil {
		assert.ErrorIs(t, sendErr, gws.ErrCloseSent, "the peer's sending must end on its own close, not on a reset")
	}
}

// A peer that has sent frames after the server's last read still receives the
// close frame, with its code and reason. The server writes the room's state
// and then the close while the peer reads nothing (requireCloseWhileSending).
func TestInteg_CloseHandshake_APeerStillSendingReceivesTheCloseCode(t *testing.T) {
	ts := refusingServer(t, textState(192<<10), nil)
	conn := dialSmallReceiveBuffer(t, ts, "room")

	doc := crdt.New(crdt.WithClientID(2))
	// Answered with the room's state, which is more than the client's receive
	// buffer holds.
	sendSync(t, conn, ygsync.EncodeSyncStep1(doc))
	// Refused, so the close is written right behind the state.
	sendSync(t, conn, ygsync.EncodeUpdate(textUpdate(doc, "refused")))
	requireCloseWhileSending(t, conn)
}

// A peer that is still reading what came before the close frame when the
// closing handshake's wait ends still receives the frame, with its code and
// reason, though it goes on sending: the server ends the connection then
// without a reset. The peer reads nothing until the wait has passed, so the
// state and the close wait in the buffers at both ends, and sends awareness
// all the while; a server that closed the socket when the wait ended would
// receive that awareness on a closed socket, which resets the connection
// (requireCloseWhileSendingAfter). A browser applying a large state, or one on
// a slow link, reaches the frame that late.
func TestInteg_CloseHandshake_APeerStillReadingWhenTheWaitEndsReceivesTheCloseCode(t *testing.T) {
	ts := refusingServer(t, textState(192<<10), nil)
	conn := dialSmallReceiveBuffer(t, ts, "room")

	doc := crdt.New(crdt.WithClientID(2))
	sendSync(t, conn, ygsync.EncodeSyncStep1(doc))
	sendSync(t, conn, ygsync.EncodeUpdate(textUpdate(doc, "refused")))
	requireCloseWhileSendingAfter(t, conn, ygws.CloseHandshakeTimeout+time.Second)
}

// A peer that never answers the close frame with its own does not hold its
// connection open: the server closes it once the closing handshake's wait of
// five seconds has passed.
func TestInteg_CloseHandshake_APeerThatNeverAnswersIsClosed(t *testing.T) {
	ts := refusingServer(t, nil, nil)
	conn := dial(t, ts, "room")
	conn.SetCloseHandler(func(int, string) error { return nil }) // sends no close in reply
	doc := crdt.New(crdt.WithClientID(2))
	drainHandshake(t, conn, doc)

	sendSync(t, conn, ygsync.EncodeUpdate(textUpdate(doc, "refused")))
	readClose(t, conn)

	requireServerCloses(t, conn)
}

// requireServerCloses requires the server to close conn, which has read the
// close frame and sends nothing more, within twice the closing handshake's
// wait.
func requireServerCloses(t *testing.T, conn *gws.Conn) {
	t.Helper()
	start := time.Now()
	require.NoError(t, conn.NetConn().SetReadDeadline(time.Now().Add(10*time.Second)))
	_, err := conn.NetConn().Read(make([]byte, 1))
	require.Error(t, err)
	assert.False(t, isTimeout(err), "the server must close a peer that never answers its close; still open after %s", time.Since(start))
}

// While the server waits for a closed peer's answer, the room goes on
// broadcasting, and frames for that peer are dropped rather than queued: the
// server writes nothing after the close frame, so they would fill the peer's
// write queue and close the connection as a slow peer's before the peer had
// answered.
func TestInteg_CloseHandshake_RoomTrafficDoesNotCloseAPeerBeingClosed(t *testing.T) {
	const queueSize = 4
	var logs serverLog
	ts := refusingServer(t, nil, func(srv *ygws.Server) {
		srv.PeerWriteQueueSize = queueSize
		srv.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	closing := dial(t, ts, "room")
	closing.SetCloseHandler(func(int, string) error { return nil }) // sends no close in reply
	doc := crdt.New(crdt.WithClientID(2))
	drainHandshake(t, closing, doc)
	sender := dial(t, ts, "room")
	drainHandshake(t, sender, crdt.New())
	watcher := dial(t, ts, "room")
	drainHandshake(t, watcher, crdt.New())
	go func() { // discards whatever the sender is sent
		for {
			if _, _, err := sender.ReadMessage(); err != nil {
				return
			}
		}
	}()

	sendSync(t, closing, ygsync.EncodeUpdate(textUpdate(doc, "refused")))
	readClose(t, closing)

	// Each awareness frame the sender sends is broadcast to the closing peer
	// too. Once the watcher has the last one, every broadcast before it has
	// been offered to the closing peer: far more than its queue holds.
	presence := awareness.New(3)
	for i := range 3 * queueSize {
		presence.SetLocalState(map[string]any{"cursor": i})
		sendAwareness(t, sender, presence.EncodeUpdate(nil))
		for {
			if outerType, _ := readOne(t, watcher, 2*time.Second); outerType == 1 { // msgAwareness
				break
			}
		}
	}
	assert.NotContains(t, logs.String(), "closing slow peer", "a peer being closed must not be closed again as a slow peer")
}

// A peer whose write queue is full when the server closes it still receives
// the close frame, with its code and reason. The close cannot then wait in the
// queue behind the frames ahead of it, so the server writes it at once and
// writes nothing of the queue after it, and must still wait for the peer's
// answer. The peer reads nothing while the server writes it a state far larger
// than the buffers at both ends take, so the server's writer blocks, and the
// room echoes the peer's awareness back to it until its queue is full; the
// server keeps a peer whose queue is full (SlowPeerResync) where the default
// would close it as a slow one.
func TestInteg_CloseHandshake_APeerWithAFullQueueReceivesTheCloseCode(t *testing.T) {
	const queueSize = 256
	ts := refusingServer(t, textState(8<<20), func(srv *ygws.Server) {
		srv.PeerWriteQueueSize = queueSize
		srv.SlowPeerPolicy = ygws.SlowPeerResync
	})
	conn := dialSmallReceiveBuffer(t, ts, "room")

	doc := crdt.New(crdt.WithClientID(2))
	sendSync(t, conn, ygsync.EncodeSyncStep1(doc))
	presence := awareness.New(2)
	for i := range 2 * queueSize {
		presence.SetLocalState(map[string]any{"cursor": i})
		sendAwareness(t, conn, presence.EncodeUpdate(nil))
	}
	// Refused while the queue is full.
	sendSync(t, conn, ygsync.EncodeUpdate(textUpdate(doc, "refused")))
	requireCloseWhileSending(t, conn)
}

// What a peer sends while the server waits for its answer to a close is
// dropped as it is read, through a small buffer, rather than read whole: a
// frame the size of the read limit would otherwise take that much memory, for
// nothing.
//
// The frame is written by hand, all but its last byte, so the server is still
// reading it when the heap is measured: a server that read it whole would then
// hold all it has read of it, which is every byte but what the buffers at both
// ends hold (4 MiB each at most here, so at least 16 MiB of a 24 MiB frame).
// The measure is the heap the process holds once a collection has run, before
// the refusal and after the frame, so what other goroutines allocate and drop
// meanwhile is not counted. The server gives the peer five seconds from its
// close frame to answer (closeHandshakeTimeout), and the frame, its
// measurement and the answer all fit in them on a machine whose CPUs are
// busy; the baseline is read before the close so its collection does not
// spend them.
func TestInteg_CloseHandshake_AFrameSentDuringTheHandshakeIsNotHeld(t *testing.T) {
	const frameBytes = 24 << 20
	ts := refusingServer(t, nil, nil)
	conn := dial(t, ts, "room")
	conn.SetCloseHandler(func(int, string) error { return nil }) // answers below, by hand
	doc := crdt.New(crdt.WithClientID(2))
	drainHandshake(t, conn, doc)
	chunk := make([]byte, 64<<10)
	before := liveHeap()
	sendSync(t, conn, ygsync.EncodeUpdate(textUpdate(doc, "refused")))
	readClose(t, conn)

	// A binary frame of frameBytes, masked with a key of zero bytes, so its
	// payload of zeros is sent as it is.
	header := make([]byte, 14)
	header[0] = 0x82       // FIN, binary
	header[1] = 0x80 | 127 // masked, 64-bit length
	binary.BigEndian.PutUint64(header[2:10], frameBytes)
	raw := conn.NetConn()
	require.NoError(t, raw.SetWriteDeadline(time.Now().Add(closeReadWait)))
	_, err := raw.Write(header)
	require.NoError(t, err)
	for left := frameBytes - 1; left > 0; left -= min(left, len(chunk)) {
		_, err := raw.Write(chunk[:min(left, len(chunk))])
		require.NoError(t, err, "the server stopped reading the frame")
	}
	after := liveHeap()
	assert.Less(t, int64(after)-int64(before), int64(frameBytes/4),
		"the server holds about as much as the frame it was to drop")

	_, err = raw.Write(chunk[:1])
	require.NoError(t, err)
	require.NoError(t, conn.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(gws.CloseNormalClosure, ""), time.Now().Add(closeReadWait)))
	// The server closes the connection once it reads the answer, which is
	// behind the frame.
	require.NoError(t, raw.SetReadDeadline(time.Now().Add(closeReadWait)))
	_, err = raw.Read(make([]byte, 1))
	require.Error(t, err)
	require.False(t, isTimeout(err), "the server did not close the connection after the peer's answer")
}

// liveHeap is the heap the process holds once a collection has run.
func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A peer that has sent frames after the server's last read still receives the
// close frame an embedder's cancel cause asks for (Server.ServeHTTP), with its
// code and reason, as it receives the close for a refused update.
func TestInteg_CloseHandshake_APeerStillSendingReceivesTheCauseCloseCode(t *testing.T) {
	ts, cancels := causeServer(t, textState(192<<10))
	conn := dialSmallReceiveBuffer(t, ts, "room")
	cancel := <-cancels

	// Answered with the room's state, which is more than the client's receive
	// buffer holds.
	sendSync(t, conn, ygsync.EncodeSyncStep1(crdt.New(crdt.WithClientID(2))))
	// Once the state is written, the close is written right behind it.
	time.Sleep(100 * time.Millisecond)
	cancel(closeCodeCause{code: refusalCode, reason: refusalReason})
	requireCloseWhileSending(t, conn)
}

// A peer that never answers the close frame for a cancel cause with its own
// does not hold its connection open either.
func TestInteg_CloseHandshake_APeerThatNeverAnswersTheCauseCloseIsClosed(t *testing.T) {
	ts, cancels := causeServer(t, nil)
	conn := dial(t, ts, "room")
	cancel := <-cancels
	conn.SetCloseHandler(func(int, string) error { return nil }) // sends no close in reply
	drainHandshake(t, conn, crdt.New(crdt.WithClientID(2)))

	cancel(closeCodeCause{code: refusalCode, reason: refusalReason})
	readClose(t, conn)
	requireServerCloses(t, conn)
}
