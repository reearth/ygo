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

// TestInteg_PeerWriteTimeout_DropsASlowReaderAtTheConfiguredDeadline checks
// that a peer whose write passes Server.PeerWriteTimeout is dropped at that
// deadline: not before it, and not at the 10 s default. Each deadline plus the
// margin stays under the default, and the 2.5 s one is past the one-second
// bound on the close frame for a cancelled request context, so a drop at
// either of those fails a case.
//
// As in TestInteg_WriterExit_TimedOutWriteTearsDownPeer, the client never
// reads the unbuffered net.Pipe, so the server's first write blocks until its
// deadline, and the client keeps sending, so only a closed connection ends the
// peer's read loop.
func TestInteg_PeerWriteTimeout_DropsASlowReaderAtTheConfiguredDeadline(t *testing.T) {
	t.Parallel()
	// The margin covers scheduling: the peer is torn down once the write
	// has failed and the read loop has seen the closed connection.
	const margin = 3 * time.Second

	for _, c := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "500ms", timeout: 500 * time.Millisecond},
		{name: "2.5s", timeout: 2500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if c.timeout+margin >= ygws.DefaultPeerWriteTimeout {
				t.Fatalf("a bound of %s does not separate the configured deadline from the %s default",
					c.timeout+margin, ygws.DefaultPeerWriteTimeout)
			}

			srv := ygws.NewServer()
			srv.PeerWriteTimeout = c.timeout
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

			start := time.Now()
			conn, _, err := pipeDialer(ln).Dial("ws://pipe/write-deadline-room", nil)
			require.NoError(t, err)
			defer conn.Close()

			// The client never reads, so the server's first write blocks
			// until its deadline, while these frames keep the read side
			// busy without overflowing the write queue.
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
				took := time.Since(start)
				if took < c.timeout {
					t.Fatalf("the peer was dropped %s after connect, before its %s deadline",
						took.Round(time.Millisecond), c.timeout)
				}
				t.Logf("peer dropped %s after connect, deadline %s",
					took.Round(time.Millisecond), c.timeout)
			case <-time.After(c.timeout + margin):
				t.Fatalf("a reader that took no frame was not dropped within %s of its %s deadline (the default is %s)",
					margin, c.timeout, ygws.DefaultPeerWriteTimeout)
			}
		})
	}
}
