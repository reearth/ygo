// Package websocket - internal regression test for the attach-rollback race:
// a room whose relay observers are registered against an attach that is
// rolled back mid-registration must not be left subscribed to nothing.
package websocket

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// registerRelayObservers can load its attach state and then be overtaken by
// a failed Start's rollback, whose detachRelayRooms pass runs before the
// observers are stored. Nothing would unsubscribe them afterwards: the room
// is already resident, so a retry never re-registers it, and every later
// edit is enqueued, finds no lane and is counted as a relay drop. The
// registrar must therefore undo its own wiring when the attach it wired to
// has gone.
func TestUnit_RegisterRelayObservers_UndoesItselfWhenItsAttachIsGone(t *testing.T) {
	srv := NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	relay := cluster.NewMemRelay()
	t.Cleanup(func() { require.NoError(t, relay.Close()) })
	require.NoError(t, srv.AttachRelay(relay))

	r, _, err := srv.getOrCreateRoom(context.Background(), "room")
	require.NoError(t, err)
	detachRelayRoom(r) // the rollback's pass over this room, before we store

	// Stand in for a registration that loaded its state and was then
	// overtaken: a state that is no longer the server's.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stale := &relayState{
		relay:    relay,
		sentinel: &relayOriginSentinel{},
		ctx:      ctx,
		cancel:   cancel,
		attached: make(chan struct{}),
	}
	close(stale.attached)
	srv.registerRelayObservers(r, "room", stale)

	r.mu.Lock()
	unsubs, lane := r.relayUnsub, r.relayLane
	r.mu.Unlock()
	require.Nil(t, unsubs, "observers wired to a rolled-back attach were left subscribed")
	require.Nil(t, lane, "a lane was installed for a rolled-back attach")

	// The observers must really be gone, not merely forgotten: an edit that
	// still reaches them is enqueued with no lane to take it, and counted.
	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))
	require.Zero(t, srv.RelayStats().Dropped)
}
