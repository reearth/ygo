package redis_test

import (
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
	ygoredis "github.com/reearth/ygo/cluster/redis"
	"github.com/reearth/ygo/crdt"
)

// fakeSink is a Sink test double that captures every Inject call. It
// satisfies the cluster.Sink contract without needing a real
// websocket.Server (those are exercised by the two-server integration
// test).
type fakeSink struct {
	mu       sync.Mutex
	injected []cluster.Inbound
	rooms    map[string]struct{} // rooms this sink claims to host
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

// newMiniRedisClient spins a miniredis server and returns a *goredis.Client
// pointed at it, along with a cleanup hook that closes both.
func newMiniRedisClient(t *testing.T) *goredis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// New must reject a nil client up front.
func TestUnit_New_RejectsNilClient(t *testing.T) {
	_, err := ygoredis.New(nil, ygoredis.Config{})
	require.ErrorIs(t, err, ygoredis.ErrNilClient)
}

// Publish before Start must return ErrRelayNotStarted (defensive — the
// AttachRelay wiring always calls Start first, but the contract should
// reject misuse explicitly).
func TestUnit_Publish_BeforeStart_ReturnsNotStarted(t *testing.T) {
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	err = relay.Publish(context.Background(), cluster.Outbound{
		Room: "r", Kind: cluster.KindSync, Data: []byte{0x01},
	})
	require.ErrorIs(t, err, ygoredis.ErrRelayNotStarted)
}

// Publish after Close must return ErrRelayClosed.
func TestUnit_Publish_AfterClose_ReturnsClosed(t *testing.T) {
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
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
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
	require.NoError(t, err)
	require.NoError(t, relay.Start(context.Background(), newFakeSink()))
	require.NoError(t, relay.Close())
	require.NoError(t, relay.Close(), "second Close must be a no-op")
}

// End-to-end on a single Relay: SUBSCRIBE via RoomActivated, PUBLISH via
// Publish, assert the message round-trips through Redis to Sink.Inject
// with the correct room / kind / data.
func TestInteg_PublishToSelf_RoundTrips(t *testing.T) {
	const room = "rt-self"
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	require.NoError(t, relay.Start(context.Background(), sink))
	relay.RoomActivated(room)
	// Give miniredis a moment to register the SUBSCRIBE so the subsequent
	// PUBLISH actually reaches us.
	time.Sleep(50 * time.Millisecond)

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	require.NoError(t, relay.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: payload,
	}))

	require.Eventually(t, func() bool {
		return sink.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"publish must round-trip back to this node's sink")

	got := sink.injectedSnapshot()[0]
	assert.Equal(t, room, got.Room)
	assert.Equal(t, cluster.KindSync, got.Kind)
	assert.Equal(t, payload, got.Data)
}

// Awareness kind round-trips with the right type.
func TestInteg_PublishAwareness_RoundTrips(t *testing.T) {
	const room = "rt-aw"
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	require.NoError(t, relay.Start(context.Background(), sink))
	relay.RoomActivated(room)
	time.Sleep(50 * time.Millisecond)

	awBytes := []byte{0x01, 0x02, 0x03}
	require.NoError(t, relay.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindAwareness, Data: awBytes,
	}))

	require.Eventually(t, func() bool {
		return sink.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond)

	got := sink.injectedSnapshot()[0]
	assert.Equal(t, cluster.KindAwareness, got.Kind)
}

// Publish on a room nobody subscribed to must not arrive at any sink —
// proves the per-room channel isolation.
func TestInteg_RoomIsolation_UnsubscribedRoomNotDelivered(t *testing.T) {
	const subscribedRoom = "alpha"
	const otherRoom = "beta"
	sink := newFakeSink(subscribedRoom)
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	require.NoError(t, relay.Start(context.Background(), sink))
	relay.RoomActivated(subscribedRoom)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, relay.Publish(context.Background(), cluster.Outbound{
		Room: otherRoom, Kind: cluster.KindSync, Data: []byte{0x01},
	}))

	// No subscription on "beta", so the publish should silently land
	// nowhere. Wait a bit to make sure nothing arrives.
	time.Sleep(200 * time.Millisecond)
	assert.Zero(t, sink.injectedCount(),
		"publish to an unsubscribed room must not reach any sink")
}

// RoomDeactivated must UNSUBSCRIBE so further publishes for that room
// don't arrive.
func TestInteg_RoomDeactivated_StopsDelivery(t *testing.T) {
	const room = "deact"
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	require.NoError(t, relay.Start(context.Background(), sink))
	relay.RoomActivated(room)
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, relay.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: []byte{0x01},
	}))
	require.Eventually(t, func() bool {
		return sink.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond)

	relay.RoomDeactivated(room)
	time.Sleep(50 * time.Millisecond)

	// Subsequent publish must not reach the sink.
	require.NoError(t, relay.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: []byte{0x02},
	}))
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, 1, sink.injectedCount(),
		"after RoomDeactivated, further publishes must not arrive")
}

// Reference-counted RoomActivated: two Activate calls + one Deactivate
// leaves the room still subscribed.
func TestInteg_RoomActivated_RefCounted(t *testing.T) {
	const room = "refct"
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	require.NoError(t, relay.Start(context.Background(), sink))
	relay.RoomActivated(room)
	relay.RoomActivated(room)
	relay.RoomDeactivated(room)
	time.Sleep(50 * time.Millisecond)

	// Still subscribed — publish must still arrive.
	require.NoError(t, relay.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: []byte{0x01},
	}))
	require.Eventually(t, func() bool {
		return sink.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"refcount must keep the subscription alive after a single Deactivate")
}

// Concurrent Publishers must not race or panic. Publishes a batch from N
// goroutines and asserts every one round-trips.
func TestInteg_ConcurrentPublishers_NoRace(t *testing.T) {
	const room = "race"
	const goroutines = 10
	const perGoroutine = 20
	sink := newFakeSink(room)
	relay, err := ygoredis.New(newMiniRedisClient(t), ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relay.Close() }()

	require.NoError(t, relay.Start(context.Background(), sink))
	relay.RoomActivated(room)
	time.Sleep(50 * time.Millisecond)

	var wg sync.WaitGroup
	var pubErrs atomic.Int32
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				err := relay.Publish(context.Background(), cluster.Outbound{
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
		return sink.injectedCount() == expected
	}, 4*time.Second, 20*time.Millisecond,
		"every concurrent publish must round-trip; got %d of %d",
		sink.injectedCount(), expected)
}
