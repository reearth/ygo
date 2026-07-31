package redis_test

import (
	"context"
	"sync"
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

	// started is closed the first time Inject enters the stall for
	// stallRoom, so a test can deterministically wait until delivery is
	// genuinely parked there (rather than guessing with a sleep) before
	// queuing more work behind it.
	started     chan struct{}
	startedOnce sync.Once
}

func newBlockingSink(stallRoom string, rooms ...string) *blockingSink {
	return &blockingSink{
		fakeSink:  newFakeSink(rooms...),
		stallRoom: stallRoom,
		release:   make(chan struct{}),
		started:   make(chan struct{}),
	}
}

func (s *blockingSink) Inject(ctx context.Context, in cluster.Inbound) error {
	if in.Room == s.stallRoom {
		s.startedOnce.Do(func() { close(s.started) })
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
// The backlog here is genuine, not assumed: delivery is wedged on the first
// payload (via blockingSink) so the payloads published after it provably
// queue up in the lane, still undelivered, at the moment RoomDeactivated
// runs. stopWorker does not drain them itself (it only signals the worker to
// stop — see its doc comment); the worker's own goroutine performs the final
// drain asynchronously once unblocked, and RoomDeactivated returns without
// waiting for that, so the assertion below has to poll rather than check
// immediately after RoomDeactivated returns.
func TestInteg_RoomDeactivated_DrainsBacklog(t *testing.T) {
	sink := newBlockingSink("room", "room")
	pub, sub := oneSubscriber(t, sink, ygoredis.Config{}, "room")
	t.Cleanup(func() {
		select {
		case <-sink.release:
		default:
			close(sink.release)
		}
	})

	publishSync(t, pub, "room", []byte{0x01})
	// Wait until delivery has genuinely parked inside Inject for msg 1 —
	// not just "the router probably queued it by now" — so the next
	// publishes are provably queuing up behind it rather than racing it.
	require.Eventually(t, func() bool {
		select {
		case <-sink.started:
			return true
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "delivery never parked on msg 1")

	const backlog = 4 // msgs 2..5, queued behind the stalled msg 1
	for i := 2; i <= backlog+1; i++ {
		publishSync(t, pub, "room", []byte{byte(i)})
	}
	// publishSync only confirms pub enqueued the outbound Publish call, not
	// that sub's router has since received and queued it — give the
	// (fast, local, miniredis) round trip a moment so 2..5 have genuinely
	// reached the lane before we deactivate. Without this, RoomDeactivated
	// could unsubscribe before all of them arrive, understating the
	// backlog rather than testing draining it.
	time.Sleep(100 * time.Millisecond)

	// Deactivate while msg 1 is still stalled and 2..5 are genuinely queued.
	sub.RoomDeactivated("room")

	// Unblock delivery; the worker's own final drain (triggered by
	// stopWorker closing w.done) must still deliver the whole backlog.
	close(sink.release)

	require.Eventually(t, func() bool {
		return sink.injectedCount() == backlog+1
	}, 2*time.Second, 10*time.Millisecond,
		"a backlog queued before deactivation must still be fully delivered")

	got := sink.injectedSnapshot()
	for i, in := range got {
		require.Equal(t, []byte{byte(i + 1)}, in.Data, "backlog must be delivered in order")
	}

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
	publishSync(t, pub, "room", []byte{0xFF})
	require.Eventually(t, func() bool {
		return sink.injectedCount() == backlog+2
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
