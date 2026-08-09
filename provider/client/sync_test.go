package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

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
		case <-time.After(5 * time.Second):
			t.Error("Connect did not return after cancel + Close")
		}
	})
}

// TestClient_SyncRoundTrip is the acceptance test for the dial/handshake/live
// sync loop (#165): a Client must converge with a real provider/websocket
// server in BOTH directions over the real y-websocket wire protocol.
//
// Direction 1 (server → client) is the handshake itself: content that existed
// on the server before the client ever dialed must arrive via SyncStep2, and
// Synced must close once it has — that is the entire promise Synced makes.
//
// Direction 2 (client → server) is the live path: an ordinary local Transact,
// made after the handshake, must reach the server without the caller doing
// anything but editing the Doc. This is the direction that would silently
// break if the OnUpdate observer were never registered, or registered before
// hydration, or if the loop never drained its outbound lane.
func TestClient_SyncRoundTrip(t *testing.T) {
	srv, ts := startServer(t)
	const room = "roundtrip"

	// Seed the server's doc through a path that already works, so this test
	// isolates the client: if the seed itself were broken, srv.Apply's own
	// tests would be failing too.
	require.NoError(t, srv.Apply(context.Background(), room,
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			// GetText outside transact: taking it inside would deadlock on the
			// doc lock Transact already holds.
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "server", nil) })
		}))

	doc := crdt.New()
	c, err := New(Options{URL: wsURL(ts, room), Doc: doc})
	require.NoError(t, err)
	connect(t, c)

	// Direction 1: the server's pre-existing state reaches the client, and
	// Synced closes to announce it.
	select {
	case <-c.Synced():
	case <-time.After(5 * time.Second):
		t.Fatal("Synced() did not close within 5s")
	}
	require.Equal(t, "server", doc.GetText("t").ToString(),
		"client doc must hold the server's pre-connect content after the handshake")

	// Direction 2: a plain local edit propagates to the server.
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "client-", nil) })

	require.Eventually(t, func() bool {
		sd := srv.GetDoc(room)
		return sd != nil && sd.GetText("t").ToString() == "client-server"
	}, 5*time.Second, 10*time.Millisecond,
		"local edit did not reach the server")
}
