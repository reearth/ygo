// Regression tests for the AttachRelay attach handshake: a relay that
// delivers traffic while AttachRelay is still running must find a server
// that is already wired, and a relay whose Start fails must leave the
// server unattached and retryable.
package websocket_test

import (
	"context"
	"errors"
	"testing"
	"time"

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

	select {
	case out := <-relay.published:
		assert.Equal(t, cluster.KindSync, out.Kind)
		assert.Equal(t, "room", out.Room)
	case <-time.After(2 * time.Second):
		t.Fatal("local edit never published: the room the relay created during " +
			"AttachRelay was left without relay wiring")
	}

	// Echo guard: the update injected during Start carried the relay
	// sentinel, so it must not have been published back.
	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, relay.published, "relay-injected update was echoed back")
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

	srv := ygws.NewServer()
	require.ErrorIs(t, srv.AttachRelay(failing), boom)

	// Unattached: a retry with a different relay is accepted, not refused
	// with ErrRelayAlreadyAttached.
	retry := cluster.NewMemRelay()
	defer func() { require.NoError(t, retry.Close()) }()
	require.NoError(t, srv.AttachRelay(retry))
}
