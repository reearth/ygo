// Regression tests for the AttachRelay attach handshake: a relay that
// delivers traffic while AttachRelay is still running must find a server
// that is already wired, and a relay whose Start fails must leave the
// server unattached and retryable.
package websocket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// injectDuringStartRelay delivers one remote update from inside Start —
// the window AttachRelay is still running. A relay attached to a bus other
// nodes already publish on does exactly this.
type injectDuringStartRelay struct {
	room      string
	data      []byte
	startErr  error
	injectErr error // written in Start, read after AttachRelay returns
	published chan cluster.Outbound
}

func (r *injectDuringStartRelay) Start(ctx context.Context, sink cluster.Sink) error {
	r.injectErr = sink.Inject(ctx, cluster.Inbound{
		Room: r.room, Kind: cluster.KindSync, Data: r.data,
	})
	return r.startErr
}

func (r *injectDuringStartRelay) Publish(_ context.Context, out cluster.Outbound) error {
	select {
	case r.published <- out:
	default:
	}
	return nil
}

func (r *injectDuringStartRelay) RoomActivated(string)   {}
func (r *injectDuringStartRelay) RoomDeactivated(string) {}
func (r *injectDuringStartRelay) Close() error           { return nil }

// remoteUpdate is a V1 update inserting text into "t" of a foreign doc.
func remoteUpdate(t *testing.T, clientID crdt.ClientID, s string) []byte {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(clientID))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, s, nil) })
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// A room the relay brings into existence during Start must come up fully
// relay-wired: its local changes publish, and the update that created it
// carries the relay sentinel, so the echo guard drops it.
func TestUnit_AttachRelay_RoomCreatedDuringStartIsWired(t *testing.T) {
	relay := &injectDuringStartRelay{
		room:      "room",
		data:      remoteUpdate(t, 1, "remote"),
		published: make(chan cluster.Outbound, 8),
	}

	srv := ygws.NewServer()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	require.NoError(t, srv.AttachRelay(relay))
	require.NoError(t, relay.injectErr)

	// The injected update landed in a room created inside the attach window.
	doc := srv.GetDoc("room")
	require.NotNil(t, doc)
	require.Equal(t, "remote", doc.GetText("t").ToString())

	// That room must be relay-wired: a local edit publishes to the relay.
	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	// Shutdown retires the lanes, drains them and joins their workers, so
	// once it returns every publish this server will ever make has been
	// made — no sleep needed, and no late one can arrive mid-assertion.
	require.NoError(t, srv.Shutdown(context.Background()))

	var published []cluster.Outbound
	for len(relay.published) > 0 {
		published = append(published, <-relay.published)
	}
	// Exactly one: the local edit. The update injected during Start carried
	// the relay sentinel, so the echo guard dropped it.
	require.Len(t, published, 1, "expected only the local edit to publish; "+
		"none means the room the relay created during AttachRelay was never "+
		"wired, two means the injected update was echoed back")

	out := published[0]
	assert.Equal(t, cluster.KindSync, out.Kind)
	assert.Equal(t, "room", out.Room)

	// The payload is that edit, not just any sync frame: replayed onto the
	// state the relay injected, it yields the edited text.
	replay := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(replay, relay.data, nil))
	require.NoError(t, crdt.ApplyUpdateV1(replay, out.Data, nil))
	assert.Equal(t, "localremote", replay.GetText("t").ToString())
}

// A Start failure must leave the server unattached, so the call can be
// retried with another relay.
func TestUnit_AttachRelay_FailedStartLeavesServerUnattached(t *testing.T) {
	boom := errors.New("relay start failed")
	failing := &injectDuringStartRelay{
		room:      "room",
		data:      remoteUpdate(t, 1, "remote"),
		startErr:  boom,
		published: make(chan cluster.Outbound, 8),
	}

	// Cleanups run LIFO, so the relay registered first is closed last: the
	// server shuts its lanes down while the relay is still open.
	retry := cluster.NewMemRelay()
	t.Cleanup(func() { require.NoError(t, retry.Close()) })
	srv := ygws.NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	require.ErrorIs(t, srv.AttachRelay(failing), boom)

	// Unattached: a retry with a different relay is accepted, not refused
	// with ErrRelayAlreadyAttached.
	require.NoError(t, srv.AttachRelay(retry))
}

// The rollback must also unwire the rooms the failed attempt created.
// Leaving their observers subscribed would feed a retired lane: every later
// local edit in that room is enqueued, finds no lane and is counted as a
// relay drop, for as long as the room stays resident.
func TestUnit_AttachRelay_FailedStartUnwiresTheRoomsItCreated(t *testing.T) {
	boom := errors.New("relay start failed")
	failing := &injectDuringStartRelay{
		room:      "room",
		data:      remoteUpdate(t, 1, "remote"),
		startErr:  boom,
		published: make(chan cluster.Outbound, 8),
	}

	retry := cluster.NewMemRelay()
	t.Cleanup(func() { require.NoError(t, retry.Close()) })
	srv := ygws.NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	require.ErrorIs(t, srv.AttachRelay(failing), boom)
	require.NotNil(t, srv.GetDoc("room"), "the failed attempt created the room")
	require.NoError(t, srv.AttachRelay(retry))

	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	assert.Zero(t, srv.RelayStats().Dropped,
		"a room left wired to the rolled-back attach drops its edits")
}
