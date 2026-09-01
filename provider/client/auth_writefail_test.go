package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
)

// withPostAuthWriteHook installs fn as loop.go's postAuthWriteHook for the
// duration of the test. fn runs on the loop goroutine, strictly after the Auth
// token frame has been written and strictly before the SyncStep1 that follows
// it — the one point where a test can make that second write fail on purpose.
func withPostAuthWriteHook(t *testing.T, fn func()) {
	t.Helper()
	orig := postAuthWriteHook
	postAuthWriteHook = fn
	t.Cleanup(func() { postAuthWriteHook = orig })
}

// rejectingRawServer is a bare y-websocket server that rejects the first frame
// it reads with a PermissionDenied reply and a 4401 close, then hard-closes the
// TCP connection (SO_LINGER 0 → RST) so a subsequent client write cannot
// succeed. It reports each Auth frame it observes on authFrames, and closes
// rejected once the hard close has happened.
func rejectingRawServer(t *testing.T, authFrames *atomic.Int32, rejected chan struct{}) *httptest.Server {
	t.Helper()
	var once bool
	up := gws.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			_ = conn.Close()
			return
		}
		// Count only Auth (tag 2) frames — that is what OnTokenAuth counts
		// on the real server, and what "was the token retried?" means.
		if mt, body, derr := decodeEnvelope(payload); derr == nil && mt == wireMsgAuth {
			_ = body
			authFrames.Add(1)
		}
		_ = conn.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarUint(wireMsgAuth)
			enc.WriteVarUint(authTypePermissionDenied)
			enc.WriteVarString("bad token")
		}))
		_ = conn.WriteControl(gws.CloseMessage,
			gws.FormatCloseMessage(wsCodeUnauthorized, "unauthorized"),
			time.Now().Add(time.Second))
		if nc := conn.UnderlyingConn(); nc != nil {
			if tc, ok := nc.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // force RST, so the client's next write fails
			}
		}
		_ = conn.Close()
		if !once {
			once = true
			close(rejected)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestClient_Auth_RejectionSurvivesHandshakeWriteFailure covers the gap that
// made TestClient_Auth_WrongTokenIsTerminal fail intermittently on CI with
// authCalls=2 (seen on v1.49.0's and v1.49.2's main runs).
//
// Both auth-rejection detectors live on the READ path: the PermissionDenied
// data frame and the 4401 close code. But the client writes the Auth token and
// then IMMEDIATELY writes SyncStep1 without waiting for a reply. If the server's
// rejection and close win that race, the SyncStep1 WRITE fails, the read loop is
// never entered, and the failure surfaces as an ordinary retryable I/O error —
// so runReconnectLoop backs off and dials again with a token already rejected.
//
// The rejection is not actually lost when that happens: a probe confirmed that
// data the peer sent before hard-closing stays readable even after the local
// write fails with EPIPE. The client was simply discarding it by returning
// early. This test pins that it no longer does.
func TestClient_Auth_RejectionSurvivesHandshakeWriteFailure(t *testing.T) {
	var authFrames atomic.Int32
	rejected := make(chan struct{})
	ts := rejectingRawServer(t, &authFrames, rejected)

	// Deterministic, not racy: block the loop goroutine between the Auth write
	// and the SyncStep1 write until the server has rejected and hard-closed, so
	// the SyncStep1 write is guaranteed to fail.
	withPostAuthWriteHook(t, func() {
		select {
		case <-rejected:
		case <-time.After(5 * time.Second):
		}
	})

	c, err := New(Options{
		URL:   "ws" + strings.TrimPrefix(ts.URL, "http") + "/room",
		Doc:   crdt.New(),
		Token: "wrong-token",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()

	var connectErr error
	select {
	case connectErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Connect did not return; the rejection was not recognised as terminal")
	}

	require.ErrorIs(t, connectErr, ErrAuthRejected,
		"a rejection already sitting in the receive buffer must be recognised even though "+
			"the SyncStep1 write failed before the read loop ran")

	// reconnectBackoffBase is 500ms, so 2s covers several would-be retries.
	time.Sleep(2 * time.Second)
	require.Equal(t, int32(1), authFrames.Load(),
		"the token was sent to the server more than once: a handshake write failure let a "+
			"rejected token reach a second attempt")
}
