package redis

// Internal tests — same package as the implementation so we can exercise
// unexported state directly. Used for:
//
//   - wire-format round-trip (T1 supporting): independent encoder/decoder
//     sanity, plus regression coverage if the format ever changes again.
//   - Publish's select arms (T1, T2): the back-pressure / context paths are
//     hard to drive deterministically through a real Redis without a fake
//     transport, but trivial when we can poke r.outbound + r.started
//     directly.

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
)

// TestUnit_WireFormat_RoundTrip exercises encode → decode for the v1.21.0
// wire format. A regression here typically means the four fields drifted
// out of sync between encoder and decoder.
func TestUnit_WireFormat_RoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		nodeID []byte
		out    cluster.Outbound
	}{
		{
			name:   "sync-with-payload",
			nodeID: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			out:    cluster.Outbound{Room: "room-1", Kind: cluster.KindSync, Data: []byte{0xDE, 0xAD, 0xBE, 0xEF}},
		},
		{
			name:   "awareness-empty-data",
			nodeID: []byte{0xFF},
			out:    cluster.Outbound{Room: "r", Kind: cluster.KindAwareness, Data: nil},
		},
		{
			name:   "unicode-room",
			nodeID: bytes.Repeat([]byte{0xAB}, 16),
			out:    cluster.Outbound{Room: "räum-™", Kind: cluster.KindSync, Data: []byte("hello, 世界")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := encodeOutbound(tc.nodeID, tc.out)
			gotNodeID, gotRoom, gotKind, gotData, err := decodeInbound(body)
			require.NoError(t, err)
			assert.Equal(t, tc.nodeID, gotNodeID)
			assert.Equal(t, tc.out.Room, gotRoom)
			assert.Equal(t, tc.out.Kind, gotKind)
			// Treat nil and empty slice as equivalent for the data field.
			if len(tc.out.Data) == 0 {
				assert.Empty(t, gotData)
			} else {
				assert.Equal(t, tc.out.Data, gotData)
			}
		})
	}
}

// TestUnit_WireFormat_DecodeShortInputs verifies decode returns a clean
// error (no panic) on truncated bytes. We tear off the tail one byte at a
// time from a known-good frame.
func TestUnit_WireFormat_DecodeShortInputs(t *testing.T) {
	good := encodeOutbound([]byte{1, 2, 3, 4}, cluster.Outbound{
		Room: "abc", Kind: cluster.KindSync, Data: []byte{0xFF, 0xEE},
	})
	for trunc := 0; trunc < len(good); trunc++ {
		_, _, _, _, err := decodeInbound(good[:trunc])
		require.Error(t, err, "decode must error on truncated input at len=%d", trunc)
	}
}

// TestUnit_Publish_BufferFull_ContextDeadline drives Publish's select
// directly. We set started=true, fill r.outbound (cap 1) without ever
// Starting the goroutines, then assert the next Publish blocks until the
// caller's ctx expires.
//
// This is T1 in the v1.21.0 review — the contract says Publish blocks on
// a full buffer until a slot frees, the ctx cancels, or the relay closes.
// Without this test the back-pressure path is unverified.
func TestUnit_Publish_BufferFull_ContextDeadline(t *testing.T) {
	r := &Relay{
		outbound: make(chan cluster.Outbound, 1),
		done:     make(chan struct{}),
		startCtx: context.Background(),
	}
	r.started.Store(true)

	// Fill the single buffer slot.
	r.outbound <- cluster.Outbound{Room: "r", Kind: cluster.KindSync, Data: []byte{0x01}}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := r.Publish(ctx, cluster.Outbound{Room: "r", Kind: cluster.KindSync, Data: []byte{0x02}})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, elapsed, 50*time.Millisecond,
		"Publish should have blocked on the full buffer until ctx expired (elapsed=%s)", elapsed)
}

// TestUnit_Publish_BufferFull_DoneClose verifies the same select arm exits
// on done close (the Close path) rather than a caller-ctx cancel.
func TestUnit_Publish_BufferFull_DoneClose(t *testing.T) {
	r := &Relay{
		outbound: make(chan cluster.Outbound, 1),
		done:     make(chan struct{}),
		startCtx: context.Background(),
	}
	r.started.Store(true)
	r.outbound <- cluster.Outbound{Room: "r"}

	// Close the relay's done channel from a goroutine after 25ms.
	go func() {
		time.Sleep(25 * time.Millisecond)
		close(r.done)
	}()

	start := time.Now()
	err := r.Publish(context.Background(), cluster.Outbound{Room: "r"})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrRelayClosed)
	require.GreaterOrEqual(t, elapsed, 20*time.Millisecond)
	require.Less(t, elapsed, 200*time.Millisecond,
		"Publish should have exited promptly on done close (elapsed=%s)", elapsed)
}

// TestUnit_Publish_BufferFull_StartCtxCancel verifies the H3 arm — startCtx
// cancellation surfaces as ErrRelayClosed even when the caller's ctx is
// still alive and the buffer is full.
func TestUnit_Publish_BufferFull_StartCtxCancel(t *testing.T) {
	startCtx, cancel := context.WithCancel(context.Background())
	r := &Relay{
		outbound: make(chan cluster.Outbound, 1),
		done:     make(chan struct{}),
		startCtx: startCtx,
	}
	r.started.Store(true)
	r.outbound <- cluster.Outbound{Room: "r"}

	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()

	err := r.Publish(context.Background(), cluster.Outbound{Room: "r"})
	require.ErrorIs(t, err, ErrRelayClosed,
		"startCtx cancellation must surface as ErrRelayClosed (H3)")
}

// TestUnit_Publish_ClosedFastPath — closed check is the first thing in
// Publish; it must short-circuit before any select work.
func TestUnit_Publish_ClosedFastPath(t *testing.T) {
	r := &Relay{
		outbound: make(chan cluster.Outbound, 1),
		done:     make(chan struct{}),
		startCtx: context.Background(),
	}
	r.started.Store(true)
	r.closed.Store(true)

	err := r.Publish(context.Background(), cluster.Outbound{Room: "r"})
	require.ErrorIs(t, err, ErrRelayClosed)
}

// TestUnit_Publish_NotStartedFastPath — same fast path for not-started.
func TestUnit_Publish_NotStartedFastPath(t *testing.T) {
	r := &Relay{
		outbound: make(chan cluster.Outbound, 1),
		done:     make(chan struct{}),
	}
	// started never set
	err := r.Publish(context.Background(), cluster.Outbound{Room: "r"})
	require.ErrorIs(t, err, ErrRelayNotStarted)
}

// TestUnit_Publish_CtxAlreadyCancelled — caller's ctx already cancelled
// must short-circuit before touching the channel.
func TestUnit_Publish_CtxAlreadyCancelled(t *testing.T) {
	r := &Relay{
		outbound: make(chan cluster.Outbound, 8),
		done:     make(chan struct{}),
		startCtx: context.Background(),
	}
	r.started.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.Publish(ctx, cluster.Outbound{Room: "r"})
	require.ErrorIs(t, err, context.Canceled)

	// Buffer must not have received the value.
	require.Empty(t, r.outbound)
}

// TestUnit_RoomActivated_GoroutineCounter — pure refcount logic, no Redis
// I/O involved beyond observing that the SAME pubSub call would have been
// made once. We use the activeRooms internal map as the assertion target.
//
// This catches refcount-arithmetic regressions without needing a broker.
func TestUnit_RoomActivated_RefcountInternal(t *testing.T) {
	r := newTestRelayNoStart()
	r.started.Store(true)
	// pubSub is nil — the only path that reaches the unguarded Subscribe
	// call is the count==1 branch. We avoid it by activating twice from
	// different "callers" without a real broker.
	//
	// Trick: use a dummy non-nil pubSub indirection via a wrapper would be
	// over-engineering; instead, observe the activeRooms map directly.
	r.mu.Lock()
	r.activeRooms["x"] = 0
	r.mu.Unlock()

	// First activate would call Subscribe (we'd panic on nil pubSub) — so
	// pre-seed count to 1 so Activate just bumps to 2.
	r.mu.Lock()
	r.activeRooms["x"] = 1
	r.mu.Unlock()

	r.RoomActivated("x") // 1→2, no Subscribe
	r.RoomActivated("x") // 2→3, no Subscribe

	r.mu.Lock()
	assert.Equal(t, 3, r.activeRooms["x"])
	r.mu.Unlock()

	r.RoomDeactivated("x") // 3→2
	r.RoomDeactivated("x") // 2→1

	r.mu.Lock()
	assert.Equal(t, 1, r.activeRooms["x"])
	r.mu.Unlock()
}

// TestUnit_RoomDeactivated_NoUnderflow — extra Deactivate calls on a zero
// counter must be safe (no negative count, no Unsubscribe RPC).
func TestUnit_RoomDeactivated_NoUnderflow(t *testing.T) {
	r := newTestRelayNoStart()
	r.started.Store(true)

	// Extra Deactivates on a fresh relay: counter is 0, must short-circuit
	// without touching pubSub (which is nil — would panic if we hit it).
	r.RoomDeactivated("never-activated")
	r.RoomDeactivated("never-activated")

	r.mu.Lock()
	_, present := r.activeRooms["never-activated"]
	r.mu.Unlock()
	assert.False(t, present, "underflowed entries must not be created")
}

// newTestRelayNoStart constructs a Relay without invoking New (which would
// require a real *goredis.Client). Used by internal tests that drive the
// state machine directly.
func newTestRelayNoStart() *Relay {
	return &Relay{
		outbound:    make(chan cluster.Outbound, 8),
		done:        make(chan struct{}),
		startCtx:    context.Background(),
		activeRooms: make(map[string]int),
	}
}

// Sanity: ensure the closed.Store path doesn't trip race detector under
// concurrent reads. This is a smoke test that complements the
// integration-level stress in redis_test.go.
func TestUnit_ClosedAtomic_NoRace(t *testing.T) {
	r := newTestRelayNoStart()
	r.started.Store(true)

	var done atomic.Bool
	const readers = 8
	doneCh := make(chan struct{})

	for i := 0; i < readers; i++ {
		go func() {
			for !done.Load() {
				_ = r.closed.Load()
				_ = r.started.Load()
			}
			doneCh <- struct{}{}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	r.closed.Store(true) // single writer
	done.Store(true)
	for i := 0; i < readers; i++ {
		<-doneCh
	}
}
