package redis_test

import (
	"context"
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
	// Default capacity, single in-flight publisher: coalescing only kicks in
	// once the lane is saturated, and even when it does, TakeSync merges the
	// backlog in FIFO order — it never reorders, so delivery order still
	// matches publish order here.
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

// A message for a room this node has not activated must still reach Inject:
// Server.Inject auto-creates the room so a node with no local peers still
// materialises converged state (provider/websocket/cluster.go:209-211).
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
