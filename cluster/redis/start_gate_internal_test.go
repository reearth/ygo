package redis

// Internal test for Publish's start gate: a Publish that races an in-flight
// Start must wait for it rather than reject the relay. Internal because the
// only deterministic way to hold Start open is to take the lock it needs —
// Start does no blocking I/O, so there is nothing slow to hook from outside.

import (
	"context"
	"sync"
	"sync/atomic"
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
	// attempt already marked in flight. Unlocking is guarded and registered
	// as a cleanup: a failure below must not leave Close, whose cleanup was
	// registered first and so runs last, deadlocked on the same mutex.
	var once sync.Once
	unlock := func() { once.Do(r.mu.Unlock) }
	r.mu.Lock()
	t.Cleanup(unlock)

	startErr := make(chan error, 1)
	go func() { startErr <- r.Start(context.Background(), &countingSink{}) }()

	// Wait until the Start attempt is visible, so this is a race with a
	// started Start rather than with a goroutine that has not run yet.
	require.Eventually(t, func() bool { return r.starting.Load() != nil },
		2*time.Second, time.Millisecond, "Start never registered an attempt")

	// released records that Start has been let go, so the publisher can say
	// whether it returned before or after — a Publish that skips the gate
	// returns while Start is still parked.
	var released atomic.Bool
	type result struct {
		err   error
		after bool
	}
	entered := make(chan struct{})
	pubErr := make(chan result, 1)
	go func() {
		close(entered)
		err := r.Publish(context.Background(), cluster.Outbound{
			Room: "room", Kind: cluster.KindSync, Data: []byte{1},
		})
		pubErr <- result{err: err, after: released.Load()}
	}()
	<-entered // the publisher is running, not merely spawned

	select {
	case res := <-pubErr:
		unlock()
		t.Fatalf("Publish returned %v during an in-flight Start; it must wait", res.err)
	case <-time.After(100 * time.Millisecond):
	}

	released.Store(true)
	unlock() // let Start finish
	require.NoError(t, <-startErr)

	select {
	case res := <-pubErr:
		require.NoError(t, res.err)
		require.True(t, res.after, "Publish returned before Start was released")
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

// A room activated before Start reaches mu must still be subscribed. The
// provider wires itself before calling Start, so getOrCreateRoom can issue
// RoomActivated in that window; dropping it would leave the room with no
// Redis subscription and nothing to repeat the call.
func TestUnit_RoomActivated_WaitsForInFlightStart(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	// Stand in for a Start that has marked its attempt but not yet taken mu:
	// the real one does both, and parking it on mu would leave which of the
	// two reaches the lock first up to the scheduler.
	att := &startAttempt{done: make(chan struct{})}
	require.True(t, r.starting.CompareAndSwap(nil, att))

	activated := make(chan struct{})
	go func() { r.RoomActivated("room"); close(activated) }()

	select {
	case <-activated:
		t.Fatal("RoomActivated returned during an in-flight Start; the activation was dropped")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, r.Start(context.Background(), &countingSink{}))
	r.starting.Store(nil)
	close(att.done)

	select {
	case <-activated:
	case <-time.After(2 * time.Second):
		t.Fatal("RoomActivated never returned after Start finished")
	}

	r.mu.Lock()
	active := r.activeRooms["room"]
	r.mu.Unlock()
	require.Equal(t, 1, active, "the room was never subscribed")
}

// awaitStart must not reject a relay that finished starting between
// Publish's started check and the attempt lookup: the attempt is cleared on
// the way out, so a nil attempt means either "never started" or "just did".
func TestUnit_AwaitStart_AcceptsARelayThatJustStarted(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Start(context.Background(), &countingSink{}))
	require.Nil(t, r.starting.Load(), "Start clears its attempt on the way out")

	require.NoError(t, r.awaitStart(context.Background()))
}
