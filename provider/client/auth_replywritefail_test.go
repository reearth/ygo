package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// withPreSyncReplyWriteHook installs fn as loop.go's preSyncReplyWriteHook for
// the calling test's duration.
func withPreSyncReplyWriteHook(t *testing.T, fn func()) {
	t.Helper()
	orig := preSyncReplyWriteHook
	preSyncReplyWriteHook = fn
	t.Cleanup(func() { preSyncReplyWriteHook = orig })
}

// rejectAfterSyncStep1Server accepts the Auth frame, then sends its OWN
// SyncStep1 — which is what makes the client write a SyncStep2 reply — and only
// then rejects and hard-closes. That ordering puts the failing write at the
// reply site rather than at either proactive handshake write.
func rejectAfterSyncStep1Server(t *testing.T, authFrames *atomic.Int32, rejected chan struct{}) *httptest.Server {
	t.Helper()
	var once sync.Once
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
		if mt, _, derr := decodeEnvelope(payload); derr == nil && mt == wireMsgAuth {
			authFrames.Add(1)
		}
		// Ask the client what it has. Answering this is the write that must fail.
		_ = conn.WriteMessage(gws.BinaryMessage,
			encodeEnvelope(wireMsgSync, ygsync.EncodeSyncStep1(crdt.New())))
		// Now reject, and make any further client write impossible.
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
				_ = tc.SetLinger(0) // RST, so the client's reply write fails
			}
		}
		_ = conn.Close()
		// sync.Once, not a plain bool: when the fix is absent the client
		// RETRIES, so a second handler goroutine reaches this line — the very
		// case these tests exist to catch. A bool would be a data race there,
		// and could double-close rejected.
		once.Do(func() { close(rejected) })
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestClient_Auth_RejectionSurvivesSyncReplyWriteFailure is the gap #238 left.
//
// #238 taught the two PROACTIVE handshake writes (the Auth frame and SyncStep1)
// to recognise a rejection sitting unread when the write fails. But the client
// also writes in RESPONSE to the server's handshake: reading the server's
// SyncStep1 makes it answer with a SyncStep2 reply, and that write is in the
// same race. When the rejection won it, the reply write failed with EPIPE,
// surfaced as an ordinary retryable I/O error, and the reconnect loop dialled
// again with a token the server had already refused.
//
// This is what still made TestClient_Auth_WrongTokenIsTerminal fail after
// #238 — reproduced at authCalls=2 on run 46 of 400, with the reported error
// reading "client: send sync step 2 reply: ... write: broken pipe".
func TestClient_Auth_RejectionSurvivesSyncReplyWriteFailure(t *testing.T) {
	var authFrames atomic.Int32
	rejected := make(chan struct{})
	ts := rejectAfterSyncStep1Server(t, &authFrames, rejected)

	// Deterministic: hold the loop goroutine just before the reply write until
	// the server has rejected and hard-closed, so that write is certain to fail.
	withPreSyncReplyWriteHook(t, func() {
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
		t.Fatal("Connect did not return; the rejection was not treated as terminal")
	}

	require.ErrorIs(t, connectErr, ErrAuthRejected,
		"a rejection already delivered by the read pump must be recognised even though the "+
			"failing write was the SyncStep2 reply rather than a proactive handshake write")

	// reconnectBackoffBase is 500ms, so 2s covers several would-be retries.
	time.Sleep(2 * time.Second)
	require.Equal(t, int32(1), authFrames.Load(),
		"the token reached a second attempt: a write failure outside the two proactive "+
			"handshake writes still let a rejected token be retried")
}
