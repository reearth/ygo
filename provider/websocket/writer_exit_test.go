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
// write times out is torn down while it keeps sending, because only a closed
// connection makes the read loop run the teardown and fire OnLastPeer.
func TestInteg_WriterExit_TimedOutWriteTearsDownPeer(t *testing.T) {
	// The margin covers scheduling.
	const teardownBound = ygws.WriteTimeout + 5*time.Second

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

	// The client never reads an unbuffered net.Pipe, so the server's first write
	// times out while slow SyncStep1 frames keep the read side busy without
	// overflowing the write queue.
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
