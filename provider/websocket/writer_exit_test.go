package websocket_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// TestInteg_WriterExit_TimedOutWriteTearsDownPeer checks that a peer whose
// write times out is torn down, even while it keeps sending.
//
// The client stops reading right after the WebSocket handshake. net.Pipe has
// no buffer, so the server's first handshake write blocks until the server's
// write deadline (writeTimeout, 10 s) expires, and the writer goroutine exits.
// The read side of the connection still works, so the client keeps sending
// SyncStep1 frames. The server must close the connection when the writer
// exits, so that the read loop runs the teardown and OnLastPeer fires. Before
// the fix, nothing closed the connection: the peer stayed in the room and its
// replies queued on a channel with no writer.
func TestInteg_WriterExit_TimedOutWriteTearsDownPeer(t *testing.T) {
	// writeTimeout in server.go is 10 s; the margin covers scheduling.
	const teardownBound = 10*time.Second + 5*time.Second

	srv := ygws.NewServer()
	lastPeer := make(chan struct{}, 1)
	srv.OnLastPeer = func(_ context.Context, _ string) {
		select {
		case lastPeer <- struct{}{}:
		default:
		}
	}

	ln := newPipeListener()
	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = ln
	ts.Start()
	defer ts.Close()

	conn, _, err := pipeDialer(ln).Dial("ws://pipe/writer-exit-room", nil)
	require.NoError(t, err)
	defer conn.Close()
	start := time.Now()

	// The client never reads. It sends a SyncStep1 frame at a steady rate;
	// each one makes the server queue a SyncStep2 reply. The rate stays far
	// below the default write queue size (512), so a queue overflow cannot
	// be what tears the peer down.
	step1 := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(0) // outer msgSync
		enc.WriteRaw(ygsync.EncodeSyncStep1(crdt.New()))
	})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			if err := conn.WriteMessage(gws.BinaryMessage, step1); err != nil {
				return
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()

	select {
	case <-lastPeer:
		t.Logf("peer torn down %s after connect", time.Since(start).Round(time.Millisecond))
	case <-time.After(teardownBound):
		t.Fatalf("peer with a timed-out write was not torn down within %s: "+
			"the writer exited but the connection stayed open", teardownBound)
	}
}
