package redis

// Internal test for Publish's start gate: a Publish that races an in-flight
// Start must wait for it rather than reject the relay. Internal because the
// only deterministic way to hold Start open is to take the lock it needs —
// Start does no blocking I/O, so there is nothing slow to hook from outside.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
)

// Publish called while Start is still running must block until Start
// finishes and then publish, not fail with ErrRelayNotStarted: Publish's
// contract rejects only a relay Start has not been called on.
//
// provider/websocket's AttachRelay commits its wiring before calling Start,
// so a room created in that window can commit a local edit and publish
// before Start has returned.
func TestUnit_Publish_WaitsForInFlightStart(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	// Hold the lock Start takes, so the Start below parks inside it with the
	// attempt already marked in flight.
	r.mu.Lock()

	startErr := make(chan error, 1)
	go func() { startErr <- r.Start(context.Background(), &countingSink{}) }()

	// Wait until the Start attempt is visible, so this is a race with a
	// started Start rather than with a goroutine that has not run yet.
	require.Eventually(t, func() bool { return r.starting.Load() != nil },
		2*time.Second, time.Millisecond, "Start never registered an attempt")

	pubErr := make(chan error, 1)
	go func() {
		pubErr <- r.Publish(context.Background(), cluster.Outbound{
			Room: "room", Kind: cluster.KindSync, Data: []byte{1},
		})
	}()

	select {
	case err := <-pubErr:
		r.mu.Unlock()
		t.Fatalf("Publish returned %v during an in-flight Start; it must wait", err)
	case <-time.After(100 * time.Millisecond):
	}

	r.mu.Unlock() // let Start finish
	require.NoError(t, <-startErr)

	select {
	case err := <-pubErr:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Publish never returned after Start finished")
	}
}

// With no Start attempt at all, Publish still fails fast — the contract for
// a relay nobody has started is unchanged.
func TestUnit_Publish_NoStartAttemptStillFailsFast(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	done := make(chan error, 1)
	go func() {
		done <- r.Publish(context.Background(), cluster.Outbound{Room: "room"})
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrRelayNotStarted)
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked although no Start was ever called")
	}
}
