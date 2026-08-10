package client

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

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

// TestClient_PersistsUpdatesReceivedFromServer covers the middle row of
// onDocUpdate's origin table — remote-origin updates are STORED (and, by the
// same rule, not sent back) — which is the row the hydrate/remote sentinel
// split exists to make expressible.
//
// The failure it guards against is quiet and total. If server-received updates
// were skipped for persistence, everything would still look correct while the
// process lived: the Doc would hold the server's content, sync would work, the
// round-trip test above would pass. The damage only shows up on the next
// offline start, when hydration returns a document containing whatever the
// user typed themselves and nothing they were ever sent — with no error
// anywhere to indicate the rest was discarded rather than never received.
// Asserting on the Doc therefore proves nothing here; the store has to be read
// back on its own terms, which is why this replays it into a fresh Doc.
//
// No polling is needed: the store write happens inside ApplySyncMessage's
// observer callback, which handleFrame completes before it calls markSynced,
// so by the time Synced() is readable the write has already returned.
func TestClient_PersistsUpdatesReceivedFromServer(t *testing.T) {
	srv, ts := startServer(t)
	const room = "remotepersist"

	require.NoError(t, srv.Apply(context.Background(), room,
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "from-server", nil) })
		}))

	store, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "remote.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	_, doc := dialSynced(t, ts, room, Options{Store: store})
	require.Equal(t, "from-server", doc.GetText("t").ToString(),
		"handshake must have delivered the server's content")

	// The real assertion: a cold start that never reaches the network must be
	// able to reconstruct that content from the store alone.
	blob, err := store.LoadDoc(room)
	require.NoError(t, err)
	require.NotEmpty(t, blob,
		"server-received update was never persisted; an offline restart would lose it")

	fresh := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(fresh, blob, nil))
	require.Equal(t, "from-server", fresh.GetText("t").ToString(),
		"store did not round-trip the server's content")
}
