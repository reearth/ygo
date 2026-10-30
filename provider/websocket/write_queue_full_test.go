package websocket_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"

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
