package client

import (
	"context"
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// hangDeadline bounds every "did the client get there?" wait in this package.
// Its ONLY job is to fail a test that has genuinely hung — and a hang never
// completes, however long you wait — so it is deliberately generous rather
// than tight. A wait returns the instant the condition holds, so a large
// bound costs a passing test nothing.
//
// It was 5s, which is marginal on CI. TestClient_Close_JoinsLoopBeforeReturning
// failed twice on main at exactly 5.01s ("client never reported state 2") on a
// 2-vCPU runner while the crdt package's ~88s CPU-saturating run overlapped
// this one. The same tree passed on the PR branch, where the whole suite ran a
// few seconds faster — the code was identical, only the contention differed
// (#243).
//
// This package already learned the same lesson once: TestClient_Auth_
// WrongTokenIsTerminal used to assert elapsed < 300ms and that flaked on a
// loaded runner at 358ms, so the assertion was deleted and its remaining bound
// documented as "deliberately generous rather than tight". These waits are the
// tight ones that had not been revisited.
const hangDeadline = 30 * time.Second

// dumpGoroutines writes every goroutine's stack into the test log. Called when
// a wait hits hangDeadline, so a genuine stall is diagnosable from CI output
// instead of just reporting that a deadline passed. This is what keeps a
// generous deadline from becoming a way of not noticing a real hang.
//
// The buffer grows until the dump fits. runtime.Stack does not report that it
// ran out of room — it fills the buffer, silently drops the rest, and returns
// len(buf), which is indistinguishable from an exact fit. Measured with 500
// parked goroutines: a 64KiB buffer returned exactly 65536 while the complete
// dump was 170,583 bytes. A fixed size would therefore truncate hardest in the
// case this exists to diagnose — a stall that leaked goroutines — so it starts
// at 1MiB (ample for a healthy run) and doubles, up to maxGoroutineDump. If
// even that is not enough the log says so, rather than quietly ending
// mid-frame (raised in review on #244).
func dumpGoroutines(t *testing.T, what string) {
	t.Helper()
	const maxGoroutineDump = 32 << 20
	for size := 1 << 20; ; size *= 2 {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < size {
			t.Logf("%s: goroutine dump at timeout follows\n%s", what, buf[:n])
			return
		}
		if size >= maxGoroutineDump {
			t.Logf("%s: goroutine dump at timeout follows, TRUNCATED at the %d-byte cap\n%s",
				what, size, buf[:n])
			return
		}
	}
}

// startServer stands up a real provider/websocket.Server behind an httptest
// server and returns both: the *ygws.Server for server-side assertions and
// injection (Apply / GetDoc / Rooms), and the *httptest.Server for its URL.
//
// Each opt is applied to the Server BEFORE it starts serving, which is the
// only window in which most of its configuration fields may be set (they are
// read unsynchronised from connection goroutines). That is what lets a test
// exercise a configured server — OnTokenAuth, Authorize, SlowPeerPolicy —
// through this harness rather than having to hand-roll its own.
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
func startServer(t *testing.T, opts ...func(*ygws.Server)) (*ygws.Server, *httptest.Server) {
	t.Helper()
	srv := ygws.NewServer()
	for _, opt := range opts {
		opt(srv)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), hangDeadline)
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

// connect runs c.Connect on its own goroutine and registers cleanup that stops
// it and waits for it to return, so a failing test can never leave a live
// socket, a read pump, or the Connect goroutine itself behind to interfere
// with the next test (or to be reported by -race after the fact).
func connect(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Connect(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = c.Close()
		select {
		case <-done:
		case <-time.After(hangDeadline):
			dumpGoroutines(t, "Connect did not return after cancel + Close")
			t.Error("Connect did not return after cancel + Close")
		}
	})
}

// statusWaiter subscribes to c's status stream NOW and returns a function that
// blocks until want has been reported (failing the test on timeout).
//
// The two-part shape is the whole point and must not be collapsed back into a
// single "wait for this status" call. OnStatus does not replay: a subscriber
// only hears what is emitted after it subscribes. So a helper that subscribed
// at the moment the test wanted to wait would lose every status the Client had
// already reached — and a Client dialing an address that refuses instantly
// reaches StateDisconnected in microseconds, well inside the window between
// starting Connect and getting round to waiting. The test would then block
// until the timeout and fail, on a schedule set by the machine's load. Taking
// the subscription before the thing that triggers the status removes the race
// rather than making it rarer:
//
//	wait := statusWaiter(t, c, StateDisconnected)
//	connect(t, c)
//	wait()
func statusWaiter(t *testing.T, c *Client, want State) (wait func()) {
	t.Helper()
	hit := make(chan struct{})
	var once sync.Once
	unsub := c.OnStatus(func(s Status) {
		if s.State == want {
			once.Do(func() { close(hit) })
		}
	})
	return func() {
		t.Helper()
		defer unsub()
		select {
		case <-hit:
		case <-time.After(hangDeadline):
			dumpGoroutines(t, fmt.Sprintf("client never reported state %v", want))
			t.Fatalf("client never reported state %v", want)
		}
	}
}

// dialSynced is the whole preamble a connected-client test needs: construct a
// Client for room on ts, start it, and block until its first handshake has
// completed. It returns the Client and the Doc it is syncing.
//
// o supplies everything except the URL, which is derived from ts and room (a
// test that wants to dial a deliberately wrong URL should not be using this
// helper). A nil o.Doc gets a fresh crdt.New(), since most tests only care
// that there IS a doc; pass one explicitly when the test needs to have edited
// it before connecting.
//
// Synced is awaited rather than merely "connected" because almost every
// assertion a test wants to make about a connected client is meaningless until
// the handshake has settled — before that, the Doc legitimately does not yet
// have the server's state, and a test asserting on it would be racing the
// handshake rather than testing anything.
func dialSynced(t *testing.T, ts *httptest.Server, room string, o Options) (*Client, *crdt.Doc) {
	t.Helper()
	if o.Doc == nil {
		o.Doc = crdt.New()
	}
	o.URL = wsURL(ts, room)

	c, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	connect(t, c)
	select {
	case <-c.Synced():
	case <-time.After(hangDeadline):
		dumpGoroutines(t, fmt.Sprintf("client for room %q did not sync", room))
		t.Fatalf("client for room %q did not sync within %s", room, hangDeadline)
	}
	return c, o.Doc
}
