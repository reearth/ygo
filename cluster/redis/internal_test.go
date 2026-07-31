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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// countingSink is a minimal cluster.Sink test double for internal-package
// tests. It is deliberately not the redis_test package's fakeSink: that type
// lives in package redis_test and is not visible from these package-redis
// test files, which is exactly why this test needs its own.
type countingSink struct {
	mu       sync.Mutex
	injected int
}

func (s *countingSink) Inject(_ context.Context, _ cluster.Inbound) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injected++
	return nil
}

func (s *countingSink) Rooms() []string { return nil }

func (s *countingSink) GetAwareness(string) (*awareness.Awareness, bool) { return nil, false }

func (s *countingSink) GetDoc(string) *crdt.Doc { return nil }

func (s *countingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.injected
}

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
		workers:     make(map[string]*roomWorker),
	}
}

// TestUnit_WorkerForInbound_MissOnInactiveRoom_DropsStray is the #187-leak
// regression test: a router-triggered miss for a room this relay is not (or
// no longer) active for must NOT create a worker. Before this fix, the
// router created a worker unconditionally on any miss, so a straggler
// message arriving after RoomDeactivated already unsubscribed would
// re-create a worker that nothing could ever reap (a later RoomDeactivated
// for the same room just no-ops, since activeRooms is already back at
// zero) — the exact unbounded per-room growth this task exists to stop.
func TestUnit_WorkerForInbound_MissOnInactiveRoom_DropsStray(t *testing.T) {
	r := newTestRelayNoStart()
	t.Cleanup(func() { close(r.done) })

	w, ok := r.workerForInbound("never-activated")
	assert.False(t, ok, "a miss for an inactive room must be reported as not-ok")
	assert.Nil(t, w)

	r.workersMu.Lock()
	_, present := r.workers["never-activated"]
	r.workersMu.Unlock()
	assert.False(t, present, "a miss for an inactive room must not create a worker")
}

// TestUnit_WorkerForInbound_MissOnActiveRoom_Drops is the mirror image of
// TestUnit_WorkerForInbound_MissOnInactiveRoom_DropsStray, confirming the
// router no longer distinguishes the two cases at all: a room this relay
// counts as active (activeRooms>0) but that has no entry in r.workers still
// drops on a miss, same as an inactive room. Before the router was simplified
// to remove lazy worker recreation, this case used to call isRoomActive +
// workerFor to recreate the worker — that was what made lazy recreation after an
// explicit reap work (see the now-renamed
// TestInteg_StopWorker_ReapedRoom_DropsUntilReactivated below). Removing
// that call means workerForInbound is a plain hit-or-drop lookup against
// r.workers only; "active on the relay" and "resident in r.workers" are no
// longer treated differently by the router.
func TestUnit_WorkerForInbound_MissOnActiveRoom_Drops(t *testing.T) {
	r := newTestRelayNoStart()
	t.Cleanup(func() { close(r.done) })
	r.mu.Lock()
	r.activeRooms["room"] = 1
	r.mu.Unlock()

	w, ok := r.workerForInbound("room")
	assert.False(t, ok, "a miss must be reported as not-ok even for an active room")
	assert.Nil(t, w)

	r.workersMu.Lock()
	_, present := r.workers["room"]
	r.workersMu.Unlock()
	assert.False(t, present, "a miss must never create a worker, active room or not")
}

// TestInteg_RunSubscriber_UnrecognisedKind_DroppedNotInjected exercises the
// real router hot path (runSubscriber -> decodeInbound -> workerForInbound ->
// Push), not a helper in isolation, because that is exactly where the bug
// lived: before this fix, an out-of-range cluster.Kind decoded off the wire
// was pushed onto the room's lane unchecked, where Lane.Push's `else` branch
// treats anything that isn't KindAwareness as a KindSync blob, and the
// worker's drainLane then injects it with a hardcoded cluster.KindSync (see
// worker.go's drainLane) — silently relabelling an unknown kind as a document
// update instead of ignoring it. A garbage blob fed to
// crdt.ApplyUpdateV1/MergeUpdatesV1 this way is not just mislabelled: per
// TestLane_MergeFailure_DoesNotLose (internal/relaylane), a blob that fails to
// merge makes every subsequent collapseLocked attempt on that room's lane
// fail too, i.e. one malformed payload can jam legitimate updates for that
// room behind it.
//
// This test publishes a payload with an out-of-range Kind through the real
// Publish -> Redis pub/sub -> runSubscriber path and asserts the actual
// observable contract: nothing reaches the Sink, and the drop is counted in
// Stats().RouterDrops (mirroring the router's existing miss-drop, per the
// fix's design) rather than silently disappearing.
func TestInteg_RunSubscriber_UnrecognisedKind_DroppedNotInjected(t *testing.T) {
	mr := miniredis.RunT(t)

	pubClient := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = pubClient.Close() })
	subClient := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = subClient.Close() })

	pub, err := New(pubClient, Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })
	require.NoError(t, pub.Start(context.Background(), &countingSink{}))

	sink := &countingSink{}
	sub, err := New(subClient, Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })
	require.NoError(t, sub.Start(context.Background(), sink))

	pub.RoomActivated("room")
	sub.RoomActivated("room")
	require.Eventually(t, func() bool {
		counts := mr.PubSubNumSub(DefaultChannelPrefix + "room")
		return counts[DefaultChannelPrefix+"room"] >= 2
	}, 2*time.Second, 5*time.Millisecond)

	// cluster.Kind is a plain int with no wire-level enum enforcement, so a
	// value outside {KindSync, KindAwareness} — e.g. a newer node's
	// not-yet-understood third Kind, or corruption — can and does appear on
	// the wire in a rolling deploy. Publish one directly.
	require.NoError(t, pub.Publish(context.Background(), cluster.Outbound{
		Room: "room", Kind: cluster.Kind(99), Data: []byte("not a real update"),
	}))

	require.Eventually(t, func() bool {
		return sub.Stats().RouterDrops >= 1
	}, 2*time.Second, 5*time.Millisecond,
		"an unrecognised kind must be dropped and counted in Stats().RouterDrops")

	// Give any wrongful delivery every chance to land, then confirm it did
	// not: this is the assertion that actually matters. Before the fix, this
	// payload was pushed to the lane and injected as a mislabelled KindSync,
	// so sink.count() would be 1 here.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, 0, sink.count(),
		"a payload with an unrecognised kind must never reach Sink.Inject")
}

// TestInteg_StopWorker_ReapedRoom_DropsUntilReactivated replaces the former
// TestInteg_StopWorker_LazyRecreateOnStillSubscribedRoom, whose name and
// assertions described behaviour the router simplification above removed.
// workerForInbound no longer creates a worker on a miss (see worker.go), so
// an explicitly reaped room — stopWorker called
// directly, leaving the Redis subscription and the activeRooms refcount
// both untouched — now DROPS inbound messages instead of lazily recreating
// its worker.
//
// RoomDeactivated's own stopWorker call always pairs with an UNSUBSCRIBE and
// a refcount drop to zero, so "subscribed and active, but workerless" can
// never legitimately arise through the exported API alone — that is why this
// test lives here (package redis, where stopWorker is reachable) rather than
// in redis_test: it calls stopWorker directly to simulate that state, then
// verifies (1) a publish while reaped is dropped, not delivered, and (2)
// only a full RoomDeactivated→RoomActivated cycle — the legitimate exported
// path, which recreates the worker before resubscribing — restores delivery.
func TestInteg_StopWorker_ReapedRoom_DropsUntilReactivated(t *testing.T) {
	mr := miniredis.RunT(t)

	pubClient := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = pubClient.Close() })
	subClient := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = subClient.Close() })

	pub, err := New(pubClient, Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })
	require.NoError(t, pub.Start(context.Background(), &countingSink{}))

	sink := &countingSink{}
	sub, err := New(subClient, Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })
	require.NoError(t, sub.Start(context.Background(), sink))

	pub.RoomActivated("room")
	sub.RoomActivated("room")
	require.Eventually(t, func() bool {
		counts := mr.PubSubNumSub(DefaultChannelPrefix + "room")
		return counts[DefaultChannelPrefix+"room"] >= 2
	}, 2*time.Second, 5*time.Millisecond)

	sub.workersMu.Lock()
	_, hadWorker := sub.workers["room"]
	sub.workersMu.Unlock()
	require.True(t, hadWorker, "precondition: RoomActivated must pre-create the worker")

	sub.stopWorker("room")

	sub.workersMu.Lock()
	_, stillThere := sub.workers["room"]
	sub.workersMu.Unlock()
	require.False(t, stillThere, "stopWorker must remove the room's worker from the map")

	// The Redis subscription and activeRooms refcount are both untouched by
	// stopWorker: publishing now must be DROPPED, not lazily recreate the
	// worker (that behaviour was removed by this task's carried-forward
	// router simplification).
	require.NoError(t, pub.Publish(context.Background(), cluster.Outbound{
		Room: "room", Kind: cluster.KindSync, Data: []byte{0x09},
	}))

	// Give the message every chance to arrive if the old lazy-recreate
	// behaviour still applied, then assert it did not: no delivery, no
	// worker resurrected by the router.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, 0, sink.count(),
		"a message for a reaped-but-still-subscribed room must be dropped, not delivered")
	sub.workersMu.Lock()
	_, recreatedByMiss := sub.workers["room"]
	sub.workersMu.Unlock()
	require.False(t, recreatedByMiss, "a router miss must never recreate a worker")
	// The real runSubscriber goroutine decoded this message and hit the
	// workerForInbound miss above via the router's actual hot path (not a
	// simulated call) — so this is a faithful check that the drop is
	// counted, not just silent.
	require.Equal(t, uint64(1), sub.Stats().RouterDrops,
		"a router-dropped message must be counted in Stats().RouterDrops")

	// Recovery requires the real exported lifecycle: a full deactivate
	// (unsubscribes; stopWorker no-ops since the worker is already gone)
	// followed by a reactivate (creates a fresh worker, then resubscribes).
	pub.RoomDeactivated("room")
	sub.RoomDeactivated("room")
	pub.RoomActivated("room")
	sub.RoomActivated("room")

	sub.workersMu.Lock()
	_, recreated := sub.workers["room"]
	sub.workersMu.Unlock()
	require.True(t, recreated, "RoomActivated after a full deactivate/reactivate cycle must recreate the worker")

	// miniredis (not real Redis) needs a settle gap after a rapid
	// unsubscribe/resubscribe cycle before it reliably delivers again — see
	// the identical note in isolation_test.go's
	// TestInteg_RoomDeactivated_DrainsBacklog; this is a test-double
	// artifact, not a cluster/redis correctness issue.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, pub.Publish(context.Background(), cluster.Outbound{
		Room: "room", Kind: cluster.KindSync, Data: []byte{0x0A},
	}))
	require.Eventually(t, func() bool {
		return sink.count() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"publish after a real deactivate/reactivate cycle must deliver")
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
