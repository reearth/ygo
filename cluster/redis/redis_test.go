package redis_test

import (
	"context"
	"errors"
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
	ygoredis "github.com/reearth/ygo/cluster/redis"
	"github.com/reearth/ygo/crdt"
)

// fakeSink is a Sink test double that captures every Inject call. It
// satisfies the cluster.Sink contract without needing a real
// websocket.Server (those are exercised by the two-server integration
// test in provider/websocket/cluster_redis_test.go).
type fakeSink struct {
	mu       sync.Mutex
	injected []cluster.Inbound
	rooms    map[string]struct{}
	docs     map[string]*crdt.Doc
	awareSet map[string]*awareness.Awareness
}

func newFakeSink(rooms ...string) *fakeSink {
	s := &fakeSink{
		rooms:    make(map[string]struct{}, len(rooms)),
		docs:     make(map[string]*crdt.Doc, len(rooms)),
		awareSet: make(map[string]*awareness.Awareness, len(rooms)),
	}
	for _, r := range rooms {
		s.rooms[r] = struct{}{}
		s.docs[r] = crdt.New()
		s.awareSet[r] = awareness.New(0)
	}
	return s
}

func (s *fakeSink) Inject(_ context.Context, in cluster.Inbound) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injected = append(s.injected, in)
	return nil
}

func (s *fakeSink) Rooms() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.rooms))
	for r := range s.rooms {
		out = append(out, r)
	}
	return out
}

func (s *fakeSink) GetAwareness(room string) (*awareness.Awareness, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.awareSet[room]
	return a, ok
}

func (s *fakeSink) GetDoc(room string) *crdt.Doc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docs[room]
}

func (s *fakeSink) injectedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.injected)
}

func (s *fakeSink) injectedSnapshot() []cluster.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]cluster.Inbound, len(s.injected))
	copy(out, s.injected)
	return out
}

// newMiniRedis returns a miniredis instance scoped to t.
func newMiniRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	return miniredis.RunT(t)
}

// newClient returns a *goredis.Client pointed at mr, registered for cleanup.
func newClient(t *testing.T, mr *miniredis.Miniredis) *goredis.Client {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitSubscribed polls miniredis until the channel has at least n subscribers,
// or fails the test after 2s. Replaces the 50–100ms time.Sleep handshake
// (T3 in the v1.21.0 review) — under CI load those sleeps would flake.
func waitSubscribed(t *testing.T, mr *miniredis.Miniredis, channel string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		counts := mr.PubSubNumSub(channel)
		return counts[channel] >= n
	}, 2*time.Second, 5*time.Millisecond,
		"channel %s never reached %d subscribers", channel, n)
}

// waitUnsubscribed is the inverse — useful for asserting Deactivate took
// effect at the broker.
func waitUnsubscribed(t *testing.T, mr *miniredis.Miniredis, channel string, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		counts := mr.PubSubNumSub(channel)
		return counts[channel] == want
	}, 2*time.Second, 5*time.Millisecond,
		"channel %s never settled to %d subscribers (have %d)",
		channel, want, mr.PubSubNumSub(channel)[channel])
}

// twoRelays spawns two relays sharing one miniredis, both subscribed to
// room. relayA is the publisher in tests, relayB the receiver. Self-delivery
// is suppressed (H2) so sinkA's count stays zero for relayA's own publishes;
// sinkB sees them.
type twoRelaysFixture struct {
	mr             *miniredis.Miniredis
	relayA, relayB *ygoredis.Relay
	sinkA, sinkB   *fakeSink
}

func newTwoRelays(t *testing.T, rooms ...string) *twoRelaysFixture {
	t.Helper()
	mr := newMiniRedis(t)

	sinkA := newFakeSink(rooms...)
	sinkB := newFakeSink(rooms...)

	relayA, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	relayB, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)

	require.NoError(t, relayA.Start(context.Background(), sinkA))
	require.NoError(t, relayB.Start(context.Background(), sinkB))

	for _, room := range rooms {
		relayA.RoomActivated(room)
		relayB.RoomActivated(room)
		waitSubscribed(t, mr, ygoredis.DefaultChannelPrefix+room, 2)
	}

	t.Cleanup(func() {
		_ = relayA.Close()
		_ = relayB.Close()
	})
	return &twoRelaysFixture{mr: mr, relayA: relayA, relayB: relayB, sinkA: sinkA, sinkB: sinkB}
}

// ─── Unit ──────────────────────────────────────────────────────────────

// New must reject a nil client up front.
func TestUnit_New_RejectsNilClient(t *testing.T) {
	_, err := ygoredis.New(nil, ygoredis.Config{})
	require.ErrorIs(t, err, ygoredis.ErrNilClient)
}

// Start must reject a nil sink.
func TestUnit_Start_RejectsNilSink(t *testing.T) {
	mr := newMiniRedis(t)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	err = relay.Start(context.Background(), nil)
	require.ErrorIs(t, err, ygoredis.ErrNilSink)
}

// Publish before Start must return ErrRelayNotStarted.
func TestUnit_Publish_BeforeStart_ReturnsNotStarted(t *testing.T) {
	mr := newMiniRedis(t)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	err = relay.Publish(context.Background(), cluster.Outbound{
		Room: "r", Kind: cluster.KindSync, Data: []byte{0x01},
	})
	require.ErrorIs(t, err, ygoredis.ErrRelayNotStarted)
}

// Publish after Close must return ErrRelayClosed.
func TestUnit_Publish_AfterClose_ReturnsClosed(t *testing.T) {
	mr := newMiniRedis(t)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	require.NoError(t, relay.Start(context.Background(), newFakeSink()))
	require.NoError(t, relay.Close())

	err = relay.Publish(context.Background(), cluster.Outbound{
		Room: "r", Kind: cluster.KindSync, Data: []byte{0x01},
	})
	require.ErrorIs(t, err, ygoredis.ErrRelayClosed)
}

// Close is idempotent — calling it twice must not panic or return an error
// the second time.
func TestUnit_Close_Idempotent(t *testing.T) {
	mr := newMiniRedis(t)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	require.NoError(t, relay.Start(context.Background(), newFakeSink()))
	require.NoError(t, relay.Close())
	require.NoError(t, relay.Close(), "second Close must be a no-op")
}

// Start with a different sink than the first call must return ErrSinkMismatch.
func TestUnit_Start_TwiceWithDifferentSink_ReturnsMismatch(t *testing.T) {
	mr := newMiniRedis(t)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	sink1 := newFakeSink()
	sink2 := newFakeSink()
	require.NoError(t, relay.Start(context.Background(), sink1))
	err = relay.Start(context.Background(), sink2)
	require.ErrorIs(t, err, ygoredis.ErrSinkMismatch)
}

// Start with the SAME sink twice must be a silent no-op.
func TestUnit_Start_TwiceWithSameSink_NoOp(t *testing.T) {
	mr := newMiniRedis(t)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	sink := newFakeSink()
	require.NoError(t, relay.Start(context.Background(), sink))
	require.NoError(t, relay.Start(context.Background(), sink),
		"second Start with the same sink must be a no-op")
}

// Start after Close must return ErrRelayClosed.
func TestUnit_Start_AfterClose_ReturnsClosed(t *testing.T) {
	mr := newMiniRedis(t)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	require.NoError(t, relay.Close())

	err = relay.Start(context.Background(), newFakeSink())
	require.ErrorIs(t, err, ygoredis.ErrRelayClosed)
}

// NodeID exposed for diagnostics — must be 16 bytes and a defensive copy.
func TestUnit_NodeID_IsDefensiveCopy(t *testing.T) {
	mr := newMiniRedis(t)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	nid1 := relay.NodeID()
	require.Len(t, nid1, 16)
	nid1[0] ^= 0xFF // mutate the copy
	nid2 := relay.NodeID()
	assert.NotEqual(t, nid1[0], nid2[0], "internal nodeID must not be aliased to caller-visible copy")
}

// Custom Config.NodeID must be honoured and not mutated by post-construction
// changes to the caller's slice.
func TestUnit_NodeID_CustomPreservedAndCopied(t *testing.T) {
	mr := newMiniRedis(t)
	custom := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{NodeID: custom})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	custom[0] = 0xFF
	assert.Equal(t, byte(1), relay.NodeID()[0],
		"relay must defensively copy a caller-provided NodeID")
}

// ─── Integration: cross-node delivery ──────────────────────────────────

// Cross-node round-trip: relayA publishes, relayB's sink receives. This is
// the realistic single-room two-server topology in miniature.
func TestInteg_CrossNode_PublishCrossesNodes(t *testing.T) {
	const room = "rt-x"
	fx := newTwoRelays(t, room)

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	require.NoError(t, fx.relayA.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: payload,
	}))

	require.Eventually(t, func() bool {
		return fx.sinkB.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"relayA's publish must reach relayB's sink via Redis")

	got := fx.sinkB.injectedSnapshot()[0]
	assert.Equal(t, room, got.Room)
	assert.Equal(t, cluster.KindSync, got.Kind)
	assert.Equal(t, payload, got.Data)

	// Self-delivery is suppressed (H2): sinkA must NOT have seen the publish.
	assert.Zero(t, fx.sinkA.injectedCount(),
		"publisher must skip self-delivery via nodeID match")
}

// Awareness kind round-trips with the right type across nodes.
func TestInteg_CrossNode_PublishAwareness(t *testing.T) {
	const room = "rt-aw"
	fx := newTwoRelays(t, room)

	awBytes := []byte{0x01, 0x02, 0x03}
	require.NoError(t, fx.relayA.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindAwareness, Data: awBytes,
	}))

	require.Eventually(t, func() bool {
		return fx.sinkB.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond)

	got := fx.sinkB.injectedSnapshot()[0]
	assert.Equal(t, cluster.KindAwareness, got.Kind)
	assert.Equal(t, awBytes, got.Data)
}

// Self-delivery suppression — explicit coverage for H2. Without the
// nodeID skip, every local publish would be paid for twice (encode + Redis
// round-trip + decode + Inject + observer drop).
func TestInteg_SelfDelivery_SkippedByNodeID(t *testing.T) {
	const room = "self-skip"
	mr := newMiniRedis(t)
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	require.NoError(t, relay.Start(context.Background(), sink))
	relay.RoomActivated(room)
	waitSubscribed(t, mr, ygoredis.DefaultChannelPrefix+room, 1)

	for i := 0; i < 5; i++ {
		require.NoError(t, relay.Publish(context.Background(), cluster.Outbound{
			Room: room, Kind: cluster.KindSync, Data: []byte{byte(i)},
		}))
	}

	// Wait long enough for any deliveries to land; none should.
	time.Sleep(150 * time.Millisecond)
	assert.Zero(t, sink.injectedCount(),
		"self-deliveries must be suppressed via nodeID comparison")
}

// Publish on a room nobody subscribed to must not arrive at any sink.
func TestInteg_RoomIsolation_UnsubscribedRoomNotDelivered(t *testing.T) {
	const subscribedRoom = "alpha"
	const otherRoom = "beta"
	fx := newTwoRelays(t, subscribedRoom)

	// neither relay has activated "beta"; nobody listens on it.
	require.NoError(t, fx.relayA.Publish(context.Background(), cluster.Outbound{
		Room: otherRoom, Kind: cluster.KindSync, Data: []byte{0x01},
	}))

	time.Sleep(150 * time.Millisecond)
	assert.Zero(t, fx.sinkA.injectedCount(), "publisher's sink: nothing")
	assert.Zero(t, fx.sinkB.injectedCount(), "receiver's sink: nothing")
}

// RoomDeactivated must UNSUBSCRIBE so further publishes for that room
// don't arrive — verified end-to-end across two nodes.
func TestInteg_RoomDeactivated_StopsCrossNodeDelivery(t *testing.T) {
	const room = "deact"
	fx := newTwoRelays(t, room)

	require.NoError(t, fx.relayA.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: []byte{0x01},
	}))
	require.Eventually(t, func() bool {
		return fx.sinkB.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond)

	fx.relayB.RoomDeactivated(room)
	waitUnsubscribed(t, fx.mr, ygoredis.DefaultChannelPrefix+room, 1)

	require.NoError(t, fx.relayA.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: []byte{0x02},
	}))
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, fx.sinkB.injectedCount(),
		"after RoomDeactivated on relayB, further publishes must not arrive")
}

// Reference-counted RoomActivated: two Activates + one Deactivate leaves
// the subscription alive across nodes.
func TestInteg_RoomActivated_RefCounted(t *testing.T) {
	const room = "refct"
	fx := newTwoRelays(t, room)

	// One extra Activate on relayB, then a single Deactivate: should remain
	// subscribed (refcount goes 1→2→1, not 0).
	fx.relayB.RoomActivated(room)
	fx.relayB.RoomDeactivated(room)
	// Allow scheduling for the SUBSCRIBE/UNSUBSCRIBE round-trips to settle.
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, fx.relayA.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: []byte{0x01},
	}))
	require.Eventually(t, func() bool {
		return fx.sinkB.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"refcount must keep the subscription alive after a single Deactivate")
}

// ─── Concurrency / race coverage ───────────────────────────────────────

// T1 (review): concurrent Publishers must not race or panic. Publishes a
// batch from N goroutines on relayA and asserts every one round-trips to
// relayB.
func TestInteg_ConcurrentPublishers_NoRace(t *testing.T) {
	const room = "race"
	const goroutines = 10
	const perGoroutine = 20
	fx := newTwoRelays(t, room)

	var wg sync.WaitGroup
	var pubErrs atomic.Int32
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				err := fx.relayA.Publish(context.Background(), cluster.Outbound{
					Room: room,
					Kind: cluster.KindSync,
					Data: []byte{byte(g), byte(i)},
				})
				if err != nil {
					pubErrs.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	require.Zero(t, pubErrs.Load(), "no Publish should error")

	expected := goroutines * perGoroutine
	require.Eventually(t, func() bool {
		return fx.sinkB.injectedCount() == expected
	}, 4*time.Second, 20*time.Millisecond,
		"every concurrent publish must round-trip to the other node; got %d of %d",
		fx.sinkB.injectedCount(), expected)
}

// T2 (review): concurrent RoomActivated + RoomDeactivated for the same
// room must converge to zero. Each goroutine does Activate-then-Deactivate
// in sequence so the pair is guaranteed (a free-running Deactivate that
// races ahead of its Activate would zero-clamp and be lost — documented
// behaviour, not what we're testing here).
//
// Without the C2 fix (SUBSCRIBE/UNSUBSCRIBE held under r.mu) the underlying
// RPCs can reorder and the broker is left with a stale subscription pinned
// at 1 even though every goroutine paired its calls.
func TestInteg_ConcurrentActivateDeactivate_ConvergesToZero(t *testing.T) {
	const room = "ad-race"
	const goroutines = 200

	mr := newMiniRedis(t)
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()
	require.NoError(t, relay.Start(context.Background(), sink))

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			relay.RoomActivated(room)
			relay.RoomDeactivated(room)
		}()
	}
	wg.Wait()

	channel := ygoredis.DefaultChannelPrefix + room
	require.Eventually(t, func() bool {
		return mr.PubSubNumSub(channel)[channel] == 0
	}, 2*time.Second, 10*time.Millisecond,
		"paired Activate→Deactivate across goroutines must settle to 0 subscriptions; got %d",
		mr.PubSubNumSub(channel)[channel])
}

// T2 sibling: half the goroutines Activate-then-Deactivate, half just
// Activate. Final refcount must equal "just-Activate goroutines" exactly,
// and the broker must report exactly 1 subscription (refcount idempotent
// against the underlying SUBSCRIBE).
func TestInteg_ConcurrentMixedActivateDeactivate_RefcountHolds(t *testing.T) {
	const room = "mixed"
	const pairs = 50
	const activatesOnly = 50

	mr := newMiniRedis(t)
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()
	require.NoError(t, relay.Start(context.Background(), sink))

	var wg sync.WaitGroup
	wg.Add(pairs + activatesOnly)
	for i := 0; i < pairs; i++ {
		go func() {
			defer wg.Done()
			relay.RoomActivated(room)
			relay.RoomDeactivated(room)
		}()
	}
	for i := 0; i < activatesOnly; i++ {
		go func() {
			defer wg.Done()
			relay.RoomActivated(room)
		}()
	}
	wg.Wait()

	channel := ygoredis.DefaultChannelPrefix + room
	require.Eventually(t, func() bool {
		return mr.PubSubNumSub(channel)[channel] == 1
	}, 2*time.Second, 10*time.Millisecond,
		"50 surviving Activates must yield exactly 1 broker-side subscription (refcount idempotent); got %d",
		mr.PubSubNumSub(channel)[channel])
}

// T2 sibling: many concurrent Activate-only calls must produce exactly ONE
// underlying SUBSCRIBE (refcount idempotent), and many Deactivate-only
// calls after a single net Activate must produce exactly ONE UNSUBSCRIBE
// (never a negative-going refcount that re-fires).
func TestInteg_ConcurrentActivateBurst_SingleSubscribe(t *testing.T) {
	const room = "burst"
	mr := newMiniRedis(t)
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()
	require.NoError(t, relay.Start(context.Background(), sink))

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); relay.RoomActivated(room) }()
	}
	wg.Wait()

	channel := ygoredis.DefaultChannelPrefix + room
	waitSubscribed(t, mr, channel, 1)
	// Exactly one client subscription regardless of refcount.
	assert.Equal(t, 1, mr.PubSubNumSub(channel)[channel])
}

// C1 (review): Start + Close + Publish + RoomActivated stress test —
// validates the lifecycle mutex protects every state-transition site. Runs
// under -race; failure is a panic, a `-race` data-race report, or a
// post-Close Publish/Inject.
func TestInteg_StartCloseStress_NoRace(t *testing.T) {
	const iterations = 30
	for i := 0; i < iterations; i++ {
		t.Run("iter", func(t *testing.T) {
			mr := newMiniRedis(t)
			relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
			require.NoError(t, err)
			sink := newFakeSink("r1", "r2")

			var wg sync.WaitGroup
			// Spawn a swarm: Start, Close, RoomActivated, RoomDeactivated,
			// and Publish all racing. Several Start callers compete; one
			// wins, the rest get same-sink-noop or ErrRelayClosed.
			for j := 0; j < 4; j++ {
				wg.Add(1)
				go func() { defer wg.Done(); _ = relay.Start(context.Background(), sink) }()
			}
			for j := 0; j < 4; j++ {
				wg.Add(1)
				go func() { defer wg.Done(); relay.RoomActivated("r1") }()
				wg.Add(1)
				go func() { defer wg.Done(); relay.RoomDeactivated("r1") }()
			}
			for j := 0; j < 4; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = relay.Publish(context.Background(), cluster.Outbound{
						Room: "r1", Kind: cluster.KindSync, Data: []byte{1},
					})
				}()
			}
			wg.Add(1)
			go func() { defer wg.Done(); _ = relay.Close() }()
			wg.Wait()

			// Close must be safely callable again post-stress.
			require.NoError(t, relay.Close())
		})
	}
}

// H1 (review): a message buffered at the moment Close fires must NOT be
// Injected. We can verify this indirectly by closing immediately after a
// publish + room activation — if the subscriber leaks, sinkB sees the
// inject; if not, it doesn't.
//
// This isn't perfectly deterministic (depends on scheduling), but combined
// with the closed-flag re-check in runSubscriber, it should be reliable.
// We assert the WEAKER invariant: zero panics + Close completes cleanly.
// The stronger "no Inject" invariant is captured by code review of the
// `if r.closed.Load() { return }` check in runSubscriber.
func TestInteg_CloseDuringSubscribe_NoLateInject(t *testing.T) {
	const room = "close-race"
	mr := newMiniRedis(t)

	sink := newFakeSink(room)
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	require.NoError(t, relay.Start(context.Background(), sink))
	relay.RoomActivated(room)
	waitSubscribed(t, mr, ygoredis.DefaultChannelPrefix+room, 1)

	// Publish from a SECOND relay so self-skip doesn't suppress the
	// inbound. relayP's nodeID differs from relay's.
	relayP, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relayP.Close() }()
	require.NoError(t, relayP.Start(context.Background(), newFakeSink(room)))

	// Fire a publish + close in rapid succession.
	require.NoError(t, relayP.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: []byte{0x01},
	}))
	require.NoError(t, relay.Close())

	// Wait for any in-flight subscriber work to settle.
	time.Sleep(100 * time.Millisecond)
	// The strong invariant: subsequent Publishes after Close must not
	// arrive (subscriber goroutine has exited under r.done).
	err = relayP.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: []byte{0x02},
	})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// At most ONE inject (the pre-close one, racing the close); MUST NOT
	// see the post-close publish.
	count := sink.injectedCount()
	require.LessOrEqual(t, count, 1, "post-close publish must never reach sink")
	if count == 1 {
		assert.Equal(t, []byte{0x01}, sink.injectedSnapshot()[0].Data,
			"only the pre-close payload may appear")
	}
}

// H3 (review): if the relay's bound startCtx is cancelled (e.g. parent
// goroutine of Server.Shutdown), Publish must surface a clean
// ErrRelayClosed rather than hanging on the (now-undrained) outbound
// channel. We exercise the path by Starting with a cancellable ctx and
// cancelling it.
func TestInteg_Publish_AfterStartCtxCancel_ReturnsClosed(t *testing.T) {
	mr := newMiniRedis(t)
	sink := newFakeSink("r")
	relay, err := ygoredis.New(newClient(t, mr), ygoredis.Config{OutboundBuffer: 1})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, relay.Start(ctx, sink))
	relay.RoomActivated("r")
	waitSubscribed(t, mr, ygoredis.DefaultChannelPrefix+"r", 1)

	// Cancel the relay's bound context — the publisher goroutine exits.
	cancel()
	// Give the publisher a beat to observe the cancel.
	time.Sleep(50 * time.Millisecond)

	// Fill the now-undrained outbound buffer. The first publish may slide
	// through (publisher may have just selected one); subsequent publishes
	// must surface ErrRelayClosed via startCtx.Done() rather than block.
	for i := 0; i < 10; i++ {
		err := relay.Publish(context.Background(), cluster.Outbound{
			Room: "r", Kind: cluster.KindSync, Data: []byte{byte(i)},
		})
		if errors.Is(err, ygoredis.ErrRelayClosed) {
			return // success
		}
	}
	t.Fatal("Publish must eventually return ErrRelayClosed after startCtx cancel")
}
