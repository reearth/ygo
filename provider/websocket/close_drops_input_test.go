package websocket_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
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

// lockedLog is a log destination that the server and the test can use at
// once.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestInteg_WriteQueueFull_DropsBufferedInput(t *testing.T) {
	// A client that sends SyncStep1 and never reads fills its write queue
	// with full-state answers, and the server closes it. Over TCP the
	// server's read buffer still holds SyncStep1 frames after the close. It
	// must answer none of them, so it logs the full queue once. (A pipe
	// connection holds no unread input, so this test uses TCP.)
	const (
		room     = "write-queue-full-drops-input"
		docBytes = 256 << 10
	)
	var logs lockedLog
	left := make(chan struct{})
	srv := ygws.NewServer()
	srv.PeerWriteQueueSize = 4
	srv.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
		txt := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, strings.Repeat("x", docBytes), nil) })
		return nil
	}
	// The client is the room's only peer, so the room is empty once its read
	// loop has exited.
	srv.OnLastPeer = func(context.Context, string) { close(left) }
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	conn := dial(t, ts, room)
	drainHandshake(t, conn, crdt.New())
	// Not sendSync: a write fails once the server closes, and the loop stops.
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // msgSync
	enc.WriteRaw(ygsync.EncodeSyncStep1(crdt.New()))
	ask := enc.Bytes()
	for range 5000 {
		if conn.WriteMessage(gws.BinaryMessage, ask) != nil {
			break
		}
	}

	select {
	case <-left:
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not close the client")
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "peer write queue full"))
}

// clientFrame encodes payload (under 64 KiB) as one masked binary WebSocket
// frame from a client. The mask key is zero, so the payload goes out
// unchanged.
func clientFrame(payload []byte) []byte {
	if len(payload) > 0xFFFF {
		panic("clientFrame: payload must be under 64 KiB")
	}
	var b bytes.Buffer
	b.WriteByte(0x82) // FIN, binary
	switch n := len(payload); {
	case n < 126:
		b.WriteByte(0x80 | byte(n))
	default:
		b.WriteByte(0x80 | 126)
		b.WriteByte(byte(n >> 8))
		b.WriteByte(byte(n))
	}
	b.Write([]byte{0, 0, 0, 0})
	b.Write(payload)
	return b.Bytes()
}

// updateFrames returns n update messages from n different clients, in one
// buffer the test writes with a single call, so the server's first read puts
// all of them in its read buffer.
func updateFrames(n int) []byte {
	var all bytes.Buffer
	for i := range n {
		d := crdt.New(crdt.WithClientID(crdt.ClientID(100 + i)))
		txt := d.GetText("t")
		d.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
		enc := encoding.NewEncoder()
		enc.WriteVarUint(0) // msgSync
		enc.WriteVarUint(ygsync.MsgUpdate)
		enc.WriteVarBytes(crdt.EncodeStateAsUpdateV1(d, nil))
		all.Write(clientFrame(enc.Bytes()))
	}
	return all.Bytes()
}

// closeHarness serves srv with a cancellable request context and counts the
// updates that reach the room. With block set, the first update waits until
// the test calls releaseFirst, so the test can close the connection while the
// rest of the updates wait in the server's read buffer.
type closeHarness struct {
	srv     *ygws.Server
	ts      *httptest.Server
	cancels chan context.CancelCauseFunc
	updates atomic.Int64
	first   chan struct{}
	release chan struct{}
	once    sync.Once
	left    chan struct{}
}

func (h *closeHarness) releaseFirst() { h.once.Do(func() { close(h.release) }) }

// recv waits for ch, failing the test after 10 s.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func newCloseHarness(t *testing.T, block bool) *closeHarness {
	t.Helper()
	h := &closeHarness{
		srv:     ygws.NewServer(),
		cancels: make(chan context.CancelCauseFunc, 1),
		first:   make(chan struct{}),
		release: make(chan struct{}),
		left:    make(chan struct{}),
	}
	h.srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error {
		if h.updates.Add(1) == 1 && block {
			close(h.first)
			<-h.release
		}
		// Ignore apply's error: a hook error closes the peer through
		// enqueueClose, which would drop the input for another reason.
		_ = apply()
		return nil
	}
	h.srv.OnLastPeer = func(context.Context, string) { close(h.left) }
	h.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		h.cancels <- cancel
		h.srv.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(h.ts.Close)
	t.Cleanup(h.releaseFirst) // runs first: never leave the handler blocked in the hook
	return h
}

// awaitServerClose reads conn until the server closes it.
func awaitServerClose(t *testing.T, conn *gws.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			require.False(t, isTimeout(err), "the server did not close the connection")
			return
		}
	}
}

func (h *closeHarness) awaitLeft(t *testing.T) {
	t.Helper()
	recv(t, h.left, "the peer to leave the room")
}

func TestInteg_CloseMessage_DropsBufferedInput(t *testing.T) {
	// A Hocuspocus close message, then updates behind it in the same read:
	// none of the updates reaches the room.
	h := newCloseHarness(t, false)
	conn := dial(t, h.ts, "close-message")
	drainHandshake(t, conn, crdt.New())
	closeMsg := encoding.NewEncoder()
	closeMsg.WriteVarUint(7) // msgClose
	frames := append(clientFrame(closeMsg.Bytes()), updateFrames(20)...)
	_, err := conn.UnderlyingConn().Write(frames)
	require.NoError(t, err)

	h.awaitLeft(t)
	assert.Zero(t, h.updates.Load(), "no update after the close message may reach the room")
}

func TestInteg_ContextCancel_DropsBufferedInput(t *testing.T) {
	// A context cancel whose cause names no close code closes the socket
	// while 19 updates wait in the read buffer: none of them reaches the room.
	h := newCloseHarness(t, true)
	conn := dial(t, h.ts, "context-cancel")
	cancel := recv(t, h.cancels, "the request")
	drainHandshake(t, conn, crdt.New())
	_, err := conn.UnderlyingConn().Write(updateFrames(20))
	require.NoError(t, err)

	recv(t, h.first, "the first update")
	cancel(errors.New("no close code"))
	awaitServerClose(t, conn)
	h.releaseFirst()

	h.awaitLeft(t)
	assert.Equal(t, int64(1), h.updates.Load(), "only the update read before the close may reach the room")
}

func TestInteg_Shutdown_DropsBufferedInput(t *testing.T) {
	// Shutdown closes the socket while 19 updates wait in the read buffer:
	// none of them reaches the room.
	h := newCloseHarness(t, true)
	conn := dial(t, h.ts, "shutdown")
	recv(t, h.cancels, "the request")
	drainHandshake(t, conn, crdt.New())
	_, err := conn.UnderlyingConn().Write(updateFrames(20))
	require.NoError(t, err)

	recv(t, h.first, "the first update")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.srv.Shutdown(context.Background())
	}()
	awaitServerClose(t, conn)
	h.releaseFirst()

	recv(t, done, "Shutdown to return")
	h.awaitLeft(t)
	assert.Equal(t, int64(1), h.updates.Load(), "only the update read before the close may reach the room")
}

func TestInteg_CloseRoomForce_DropsBufferedInput(t *testing.T) {
	// CloseRoom with force closes the socket while 19 updates wait in the
	// read buffer: none of them reaches the room.
	h := newCloseHarness(t, true)
	conn := dial(t, h.ts, "close-room")
	recv(t, h.cancels, "the request")
	drainHandshake(t, conn, crdt.New())
	_, err := conn.UnderlyingConn().Write(updateFrames(20))
	require.NoError(t, err)

	recv(t, h.first, "the first update")
	done := make(chan error, 1)
	go func() { done <- h.srv.CloseRoom("close-room", true) }()
	awaitServerClose(t, conn)
	h.releaseFirst()

	require.NoError(t, recv(t, done, "CloseRoom to return"))
	assert.Equal(t, int64(1), h.updates.Load(), "only the update read before the close may reach the room")
}
