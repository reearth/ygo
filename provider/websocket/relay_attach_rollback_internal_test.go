// Package websocket - internal regression test for the attach-rollback race:
// a room whose relay observers are registered against an attach that is
// rolled back mid-registration must not be left subscribed to nothing.
package websocket

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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

// acceptingFailedStartRelay fails its Start but accepts every Publish,
// returning nil without delivering anywhere — what a relay that never
// finished starting does with a payload handed to it.
type acceptingFailedStartRelay struct {
	entered   chan struct{}
	release   chan struct{}
	published chan cluster.Outbound
}

func (r *acceptingFailedStartRelay) Start(context.Context, cluster.Sink) error {
	close(r.entered)
	<-r.release
	return errors.New("relay start failed")
}

func (r *acceptingFailedStartRelay) Publish(_ context.Context, out cluster.Outbound) error {
	select {
	case r.published <- out:
	default:
	}
	return nil
}

func (r *acceptingFailedStartRelay) RoomActivated(string)   {}
func (r *acceptingFailedStartRelay) RoomDeactivated(string) {}
func (r *acceptingFailedStartRelay) Close() error           { return nil }

// A payload held on a lane through the attach window must not be published
// once that attach has been rolled back: the relay never started, so a
// Publish it accepts goes nowhere, and an accepted Publish is not counted —
// the loss would vanish from RelayStats entirely.
//
// Internal because the ordering is what is under test. Holding relayLanesMu
// parks the rollback at its first step, so a lane released before the state
// was cleared has unbounded time to publish: without that the worker loses
// the race every run and the test passes against the defect.
func TestUnit_AttachRelay_FailedStartDoesNotPublishTheHeldBacklog(t *testing.T) {
	relay := &acceptingFailedStartRelay{
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		published: make(chan cluster.Outbound, 8),
	}

	srv := NewServer()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	attached := make(chan error, 1)
	go func() { attached <- srv.AttachRelay(relay) }()
	<-relay.entered // the state is published; Start is parked

	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	// Park the rollback before it can clear the state, then let Start fail.
	// Guarded and registered as a cleanup: the assertion below runs while
	// the lock is held, and a failure there must not leave Shutdown — whose
	// cleanup was registered first, so it runs last — deadlocked on it.
	var once sync.Once
	unlock := func() { once.Do(srv.relayLanesMu.Unlock) }
	srv.relayLanesMu.Lock()
	t.Cleanup(unlock)
	close(relay.release)
	// Long enough for a lane released ahead of the clear to publish.
	time.Sleep(100 * time.Millisecond)
	require.Empty(t, relay.published,
		"the backlog was published to a relay whose Start failed")
	unlock()

	require.Error(t, <-attached)
	require.NoError(t, srv.Shutdown(context.Background()))

	require.Empty(t, relay.published,
		"the backlog was published to a relay whose Start failed")
	require.Equal(t, uint64(1), srv.RelayStats().Dropped,
		"the edit is lost, so it must be counted")
}
