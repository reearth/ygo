package websocket_test

import (
	"net"
	"net/http/httptest"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	ygws "github.com/reearth/ygo/provider/websocket"
)

// #51 — a peer sending faster than MessageRateLimit is disconnected. Disconnect
// (rather than silently dropping the offending message) is the intended
// behaviour: a dropped CRDT update would leave the peer permanently diverged.
func TestInteg_MessageRateLimit_DisconnectsFloodingPeer(t *testing.T) {
	srv := ygws.NewServer()
	srv.MessageRateLimit = rate.Limit(2) // 2 msg/sec sustained
	srv.MessageRateBurst = 2
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "flood")
	defer conn.Close()

	// Flood well past the burst within the same second (Ping frames, tag 9).
	for i := 0; i < 100; i++ {
		if err := conn.WriteMessage(gws.BinaryMessage, []byte{0x09}); err != nil {
			break // server may have already closed the connection mid-flood
		}
	}

	// The server must disconnect us. Read until an error: a close/EOF error means
	// disconnected (pass); a read-deadline timeout means we were NOT disconnected.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var readErr error
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			readErr = err
			break
		}
	}
	require.Error(t, readErr)
	if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
		t.Fatalf("flooding peer was not disconnected (read timed out): %v", readErr)
	}
}

// #51 — the zero value (no MessageRateLimit) preserves current behaviour: a peer
// may send many messages without being disconnected.
func TestInteg_MessageRateLimit_ZeroValueUnlimited(t *testing.T) {
	srv := ygws.NewServer() // MessageRateLimit unset → unlimited
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "unlimited")
	defer conn.Close()

	for i := 0; i < 100; i++ {
		require.NoError(t, conn.WriteMessage(gws.BinaryMessage, []byte{0x09}))
	}

	// Still connected: reading a frame succeeds (handshake / Pong) rather than
	// returning a disconnect error.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	require.NoError(t, err, "peer with no rate limit should stay connected")
}
