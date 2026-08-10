package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestClient_Keepalive_DeadPeerTriggersDisconnect is #165 Task 7's central
// claim: a half-open connection — the peer vanished without ever sending a
// FIN or RST (a NAT timeout, a closed laptop lid, a network partition) —
// never surfaces as a read error on its own. Without an application-level
// ping/pong and a read deadline that expires when a pong goes unanswered,
// this client's read pump would block in ReadMessage forever, the loop
// would never return, runReconnectLoop would never get a chance to retry,
// and the client would sit there believing itself connected while syncing
// nothing. This test builds exactly that dead peer — a server that
// completes the WebSocket handshake and then never says another word, not
// even a pong — and requires the client to notice and report
// StateDisconnected within a bound generous enough to be reliable but tight
// enough that only the keepalive mechanism (not some unrelated timeout)
// could have produced it.
//
// # Why the test server never calls a Read method at all
//
// gorilla only ever invokes a registered ping/pong/close handler — including
// its OWN default ping handler, which auto-replies to an inbound ping with a
// pong — from inside NextReader, ReadMessage, or a message Reader's Read
// call (see gorilla's conn.go doc comments on SetPingHandler/SetPongHandler:
// "The handler function is called from the NextReader, ReadMessage and
// message reader Read methods"). A server goroutine that upgrades the
// connection and then never calls any of those methods has, structurally,
// no code path left that could ever answer a ping — not "probably won't",
// but cannot, regardless of what handler gorilla would otherwise have
// installed by default. That is a stronger guarantee than overriding
// SetPingHandler to a no-op would be, since the override still requires
// trusting it was wired up before any read happens; here there is no read
// to race against in the first place. (The task brief's other suggested
// option — override the handler while still reading — works too, but is
// not needed: never reading is the simpler, structurally-provable choice.)
//
// The server holds the raw net.Conn open (rather than closing it) until the
// test tears down, specifically so the failure mode under test is silence,
// not a clean close: a closed connection would surface as an ordinary read
// error immediately, which every other reconnect test in this package
// already covers and which is NOT what keepalive exists to catch.
func TestClient_Keepalive_DeadPeerTriggersDisconnect(t *testing.T) {
	upgrader := gws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverDone := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Deliberately no ReadMessage, no WriteMessage, nothing: see the test
		// doc above for why this is a structural (not merely configured)
		// guarantee that no pong is ever sent. Block until the test says to
		// stop, then close — closing only AFTER the assertion below has
		// already passed, so it plays no part in the disconnect being
		// tested.
		<-serverDone
		_ = conn.Close()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		close(serverDone)
		ts.Close()
	})

	const pingInterval = 100 * time.Millisecond
	c, err := New(Options{
		URL:          "ws" + strings.TrimPrefix(ts.URL, "http") + "/dead-peer",
		Doc:          crdt.New(),
		PingInterval: pingInterval,
		MaxBackoff:   time.Second,
	})
	require.NoError(t, err)

	// Arm the subscription BEFORE connect starts the dial, per this
	// package's documented discipline (harness_test.go's statusWaiter doc):
	// OnStatus never replays, so subscribing after the triggering action
	// risks missing a fast transition entirely.
	hit := make(chan struct{})
	var once sync.Once
	unsub := c.OnStatus(func(s Status) {
		if s.State == StateDisconnected {
			once.Do(func() { close(hit) })
		}
	})
	t.Cleanup(unsub)

	connect(t, c)

	// A generous bound relative to the mechanism under test: the read
	// deadline is 2x PingInterval (200ms here), so a correct implementation
	// disconnects at roughly that mark. 10x PingInterval (1s) gives an order
	// of magnitude of headroom for scheduler noise while still failing fast,
	// and — crucially — failing at all, rather than hanging the test binary
	// forever, which is what happens pre-keepalive: conn.ReadMessage blocks
	// with no deadline, the loop's select never returns, and nothing ever
	// reaches StateDisconnected.
	select {
	case <-hit:
	case <-time.After(10 * pingInterval):
		t.Fatal("client never reported StateDisconnected after its peer went silent " +
			"and stopped answering pings; the read deadline does not appear to be firing")
	}
}
