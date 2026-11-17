// Package websocket - internal regression test for held activations across a
// room replacement: a room torn down while its RoomActivated is still held
// back must not take its successor's activation with it.
package websocket

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
)

// heldActivationRelay holds the attach window open until released and records
// the room callbacks it is offered.
type heldActivationRelay struct {
	entered chan struct{}
	release chan struct{}

	mu          sync.Mutex
	activated   []string
	deactivated []string
}

func (r *heldActivationRelay) Start(context.Context, cluster.Sink) error {
	close(r.entered)
	<-r.release
	return nil
}

func (r *heldActivationRelay) Publish(context.Context, cluster.Outbound) error { return nil }

func (r *heldActivationRelay) RoomActivated(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activated = append(r.activated, room)
}

func (r *heldActivationRelay) RoomDeactivated(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deactivated = append(r.deactivated, room)
}

func (r *heldActivationRelay) Close() error { return nil }

func (r *heldActivationRelay) calls() (activated, deactivated []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.activated...), append([]string(nil), r.deactivated...)
}

// A room leaves s.rooms before its teardown runs — the teardown waits for the
// room's persistence goroutine to drain first — so a replacement under the
// same name can be created, and hold its own activation, while the first
// room's teardown is still pending. That teardown must settle its own
// instance's activation and leave the replacement's alone; otherwise the
// replacement is never offered to the relay and, with a relay that subscribes
// per room, stays unsubscribed for as long as it is resident.
func TestUnit_AttachRelay_ReplacementRoomKeepsItsHeldActivation(t *testing.T) {
	relay := &heldActivationRelay{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	srv := NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	attached := make(chan error, 1)
	go func() { attached <- srv.AttachRelay(relay) }()
	<-relay.entered

	ctx := context.Background()
	first, _, err := srv.getOrCreateRoom(ctx, "room")
	require.NoError(t, err)

	// The first room is evicted: out of s.rooms, teardown still to come.
	srv.rmu.Lock()
	delete(srv.rooms, "room")
	srv.rmu.Unlock()

	second, created, err := srv.getOrCreateRoom(ctx, "room")
	require.NoError(t, err)
	require.True(t, created, "the replacement room was not created")
	require.NotSame(t, first, second)

	// The first room's teardown completes now, behind its replacement.
	srv.teardownRelayRoom(first, "room")

	close(relay.release)
	require.NoError(t, <-attached)

	activated, deactivated := relay.calls()
	require.Equal(t, []string{"room"}, activated,
		"the resident replacement room was never activated")
	require.Empty(t, deactivated,
		"a room whose activation was never issued was deactivated")
}
