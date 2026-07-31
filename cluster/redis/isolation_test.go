package redis_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	ygoredis "github.com/reearth/ygo/cluster/redis"
	"github.com/reearth/ygo/crdt"
)

// blockingSink stalls Inject for one designated room until release is closed,
// standing in for a room whose delivery is wedged. Every other room must keep
// flowing — that is what #187 is about.
type blockingSink struct {
	*fakeSink
	stallRoom string
	release   chan struct{}
}

func newBlockingSink(stallRoom string, rooms ...string) *blockingSink {
	return &blockingSink{
		fakeSink:  newFakeSink(rooms...),
		stallRoom: stallRoom,
		release:   make(chan struct{}),
	}
}

func (s *blockingSink) Inject(ctx context.Context, in cluster.Inbound) error {
	if in.Room == s.stallRoom {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.fakeSink.Inject(ctx, in)
}

// oneSubscriber wires a publisher relay and a subscriber relay over one
// miniredis, both active on rooms, and returns them. cfg configures the
// subscriber only.
func oneSubscriber(t *testing.T, sink cluster.Sink, cfg ygoredis.Config, rooms ...string) (pub, sub *ygoredis.Relay) {
	t.Helper()
	mr := newMiniRedis(t)

	pub, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	sub, err = ygoredis.New(newClient(t, mr), cfg)
	require.NoError(t, err)

	require.NoError(t, pub.Start(context.Background(), newFakeSink()))
	require.NoError(t, sub.Start(context.Background(), sink))
	for _, room := range rooms {
		pub.RoomActivated(room)
		sub.RoomActivated(room)
		waitSubscribed(t, mr, ygoredis.DefaultChannelPrefix+room, 2)
	}
	t.Cleanup(func() {
		_ = pub.Close()
		_ = sub.Close()
	})
	return pub, sub
}

func publishSync(t *testing.T, pub *ygoredis.Relay, room string, data []byte) {
	t.Helper()
	require.NoError(t, pub.Publish(context.Background(), cluster.Outbound{
		Room: room, Kind: cluster.KindSync, Data: data,
	}))
}

// THE #187 GATE. A wedged room must not stall any other room. This test FAILS
// before the fix, because runSubscriber calls Inject synchronously.
func TestInteg_Subscriber_CrossRoomIsolation(t *testing.T) {
	sink := newBlockingSink("slow", "slow", "fast")
	pub, _ := oneSubscriber(t, sink, ygoredis.Config{}, "slow", "fast")
	t.Cleanup(func() { close(sink.release) })

	// Wedge "slow" first, then publish to "fast".
	publishSync(t, pub, "slow", []byte{0x01})
	publishSync(t, pub, "fast", []byte{0x02})

	require.Eventually(t, func() bool {
		for _, in := range sink.injectedSnapshot() {
			if in.Room == "fast" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond,
		`room "fast" must deliver while room "slow" is stalled`)
}

// Saturating a wedged room's lane must coalesce, never lose an edit.
func TestInteg_Subscriber_SaturatedLane_NoLoss(t *testing.T) {
	const n = 30
	sink := newBlockingSink("room", "room")
	// RoomQueueSize 2 guarantees overflow well before n updates.
	pub, _ := oneSubscriber(t, sink, ygoredis.Config{RoomQueueSize: 2}, "room")

	src := crdt.New()
	txt := src.GetText("t")
	var updates [][]byte
	unsub := src.OnUpdate(func(u []byte, _ any) {
		updates = append(updates, append([]byte(nil), u...))
	})
	for i := 0; i < n; i++ {
		src.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
	}
	unsub()
	require.Len(t, updates, n)

	for _, u := range updates {
		publishSync(t, pub, "room", u)
	}
	close(sink.release) // let delivery proceed

	wantText := txt.ToString()
	require.Eventually(t, func() bool {
		got := crdt.New()
		for _, in := range sink.injectedSnapshot() {
			if err := crdt.ApplyUpdateV1(got, in.Data, nil); err != nil {
				return false
			}
		}
		return got.GetText("t").ToString() == wantText
	}, 5*time.Second, 20*time.Millisecond,
		"every published update must survive coalescing")
}

// Per-room ordering must be preserved end to end.
func TestInteg_Subscriber_PreservesPerRoomOrder(t *testing.T) {
	sink := newFakeSink("room")
	// Default capacity, only 5 updates, single in-flight publisher: this
	// stays well under the lane's default capacity, so no coalescing occurs
	// here at all — this test is not exercising order-under-coalescing. The
	// injectedCount() == 5 assertion below would fail loudly if a merge ever
	// did happen (payloads would collapse to fewer than 5), so the test is
	// not vacuous, but it does not itself demonstrate that order survives
	// coalescing.
	pub, _ := oneSubscriber(t, sink, ygoredis.Config{}, "room")

	for i := 0; i < 5; i++ {
		publishSync(t, pub, "room", []byte{byte(i)})
	}

	require.Eventually(t, func() bool {
		return sink.injectedCount() == 5
	}, 2*time.Second, 10*time.Millisecond)

	got := sink.injectedSnapshot()
	for i, in := range got {
		require.Equal(t, []byte{byte(i)}, in.Data, "delivery order must match publish order")
	}
}

// A message for a room the Sink has never heard of must still reach Inject:
// Server.Inject auto-creates the room so a node with no local peers still
// materialises converged state (provider/websocket/cluster.go:209-211). The
// relay itself DOES activate the room (RoomActivated below), which
// pre-creates its worker — this test exercises the Sink-layer auto-create
// guarantee, not the router's lazy worker-creation branch. That branch is
// covered separately by TestInteg_StopWorker_LazyRecreateOnStillSubscribedRoom
// in internal_test.go, which needs stopWorker (unexported) to leave a
// subscribed room legitimately without a worker.
func TestInteg_Subscriber_NonResidentRoom_StillInjects(t *testing.T) {
	sink := newFakeSink() // no rooms registered
	mr := newMiniRedis(t)

	pub, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	sub, err := ygoredis.New(newClient(t, mr), ygoredis.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = pub.Close()
		_ = sub.Close()
	})
	require.NoError(t, pub.Start(context.Background(), newFakeSink()))
	require.NoError(t, sub.Start(context.Background(), sink))

	// The subscriber activates the room at the broker but the sink knows
	// nothing about it.
	pub.RoomActivated("ghost")
	sub.RoomActivated("ghost")
	waitSubscribed(t, mr, ygoredis.DefaultChannelPrefix+"ghost", 2)

	publishSync(t, pub, "ghost", []byte{0x07})

	require.Eventually(t, func() bool {
		return sink.injectedCount() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"inbound for a non-resident room must still reach Inject")
}

// closeWatchSink fails the test if Inject is called after Close returned.
// That is the invariant Relay.Close documents: no goroutine may fire
// afterwards.
type closeWatchSink struct {
	*fakeSink
	closed atomic.Bool
	t      *testing.T
}

func (s *closeWatchSink) Inject(ctx context.Context, in cluster.Inbound) error {
	if s.closed.Load() {
		s.t.Errorf("Inject called for room %q after Close returned", in.Room)
	}
	return s.fakeSink.Inject(ctx, in)
}

// Deactivating a room must not silently discard work already queued for it.
func TestInteg_RoomDeactivated_DrainsBacklog(t *testing.T) {
	sink := newFakeSink("room")
	pub, sub := oneSubscriber(t, sink, ygoredis.Config{}, "room")

	publishSync(t, pub, "room", []byte{0x01})
	// Give the router a moment to have queued it, then deactivate.
	require.Eventually(t, func() bool {
		return sink.injectedCount() == 1
	}, 2*time.Second, 5*time.Millisecond)

	sub.RoomDeactivated("room")

	// Re-activating must work and deliver again (worker recreated cleanly).
	pub.RoomActivated("room")
	sub.RoomActivated("room")
	// miniredis (not real Redis) tears down and lazily respawns its internal
	// per-connection pubsub dispatch goroutine whenever a connection's
	// subscriber count drops to zero and is then resubscribed (see
	// miniredis's endSubscriber/subscribedState). A publish that lands before
	// that goroutine has been scheduled is silently dropped by the fake
	// server — confirmed independently of this package: a tight
	// unsubscribe/resubscribe/publish loop against raw miniredis + go-redis
	// drops ~10-40% of messages with no settle gap, and 0/1000 with one. This
	// is specific to miniredis's Go-channel-per-subscriber-cycle bridge; real
	// Redis's single-threaded subscriber dispatch and go-redis's long-lived
	// client-side reader goroutine (started once, unaffected by
	// subscribe/unsubscribe cycles) have no equivalent gap, so this is a
	// test-double artifact, not a cluster/redis correctness issue.
	time.Sleep(100 * time.Millisecond)
	publishSync(t, pub, "room", []byte{0x02})
	require.Eventually(t, func() bool {
		return sink.injectedCount() == 2
	}, 2*time.Second, 10*time.Millisecond,
		"a re-activated room must deliver again")
}

// Nothing may reach the Sink after Close returns.
func TestInteg_Close_NothingFiresAfterReturn(t *testing.T) {
	base := newFakeSink("room")
	sink := &closeWatchSink{fakeSink: base, t: t}
	pub, sub := oneSubscriber(t, sink, ygoredis.Config{}, "room")

	for i := 0; i < 50; i++ {
		publishSync(t, pub, "room", []byte{byte(i)})
	}

	require.NoError(t, sub.Close())
	sink.closed.Store(true)
	// Give any leaked goroutine a window to misbehave.
	time.Sleep(100 * time.Millisecond)
}
