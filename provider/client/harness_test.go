package client

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ygws "github.com/reearth/ygo/provider/websocket"
)

// startServer stands up a real provider/websocket.Server behind an httptest
// server and returns both: the *ygws.Server for server-side assertions and
// injection (Apply / GetDoc / Rooms), and the *httptest.Server for its URL.
//
// This deliberately exercises ygo's own server rather than a hand-rolled fake
// (#165). The client's whole reason to exist is interoperating with a
// y-websocket server over the real wire protocol; a fake would let a framing
// or handshake-ordering mistake pass every test in this package and only fail
// against a real deployment. It is also cheap — httptest is in-process, so a
// full dial + handshake round-trip costs well under a millisecond.
//
// Teardown order is load-bearing: Shutdown first, ts.Close second. Shutdown
// closes every live peer connection, which lets the hijacked WebSocket HTTP
// handlers return; httptest.Server.Close blocks waiting for exactly those
// outstanding handlers, so closing in the other order would stall teardown
// until the test binary's own timeout.
func startServer(t *testing.T) (*ygws.Server, *httptest.Server) {
	t.Helper()
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		ts.Close()
	})
	return srv, ts
}

// wsURL builds the ws:// URL a Client should dial to reach room on ts. The
// final path segment is the room name, matching both roomFromURL's extraction
// rule client-side and provider/websocket.Server.ServeHTTP's path.Base fallback
// server-side — the two halves of the same convention.
func wsURL(ts *httptest.Server, room string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + room
}
