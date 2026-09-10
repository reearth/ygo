// Package-internal: pins two cluster.Relay/cluster.Sink contract obligations
// (cluster/relay.go) for the streams tier that are easy to satisfy by accident
// and easy to break silently later.
//
// The tier's other contract obligations are already pinned elsewhere:
// activation refcounting by TestUnit_StreamReader_ActivationRefcounts and
// TestInteg_RoomActivated_RefCounted; Close not hanging by
// TestUnit_StreamReader_CloseDoesNotWaitOutReadBlock,
// TestUnit_StreamTrim_CloseDoesNotHang and
// TestInteg_Close_NothingFiresAfterReturn; Publish-after-Close by
// TestUnit_Publish_AfterClose_ReturnsClosed, whose closed.Load() check runs
// before Transport is ever read.
package redis

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// RoomDeactivated's own contract (cluster/relay.go) warns that a relay
// releasing per-room PUBLISH-side state there — it names "a stream key" —
// would drop a trailing update, because the provider's lane worker can still
// call Publish for a room after RoomDeactivated has returned (the teardown
// path stops the outbound lane asynchronously and does not wait for its
// final drain).
//
// publishStream never consults streamRooms (see RoomDeactivated's doc), so
// this holds today by construction — but nothing before this test pinned it:
// grep confirms no existing test calls Publish/publishStream for a room
// after RoomDeactivated has run for it.
//
// Verified by mutation: temporarily added `if r.streamRooms[out.Room] == 0 {
// return ErrRelayClosed }` at the top of publishStream (guarded under
// streamMu) to simulate a relay that treats RoomDeactivated as releasing
// publish-side state. This test failed with exactly that error; the
// existing suite was otherwise unaffected. Reverted.
func TestUnit_StreamLifecycle_PublishWorksAfterRoomDeactivated(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	r.RoomActivated("room1")
	r.RoomDeactivated("room1")

	require.NoError(t, r.Publish(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("trailing"),
	}), "a trailing publish after deactivation must still land")

	require.Len(t, streamEntries(t, mr, r.scfg.syncKey("room1")), 1)
}

// serialGuardSink wraps countingSink's bookkeeping with a same-room
// concurrent-Inject detector, standing in for the "no two Inject calls for
// the same room overlap" half of Sink.Inject's contract
// (cluster/relay.go). Package-redis doubles available were countingSink and
// recordingSink (streams_reader_test.go); neither tracks in-flight Inject
// calls, so this is a purpose-built addition rather than a fourth
// near-duplicate of an existing sink — it is a thin wrapper, not a fresh
// fakeSink.
type serialGuardSink struct {
	t        *testing.T
	inFlight int32 // 0 or 1 in the absence of a bug; guarded via CAS below
	seen     int32
}

func (s *serialGuardSink) Inject(_ context.Context, in cluster.Inbound) error {
	if !atomic.CompareAndSwapInt32(&s.inFlight, 0, 1) {
		s.t.Errorf("concurrent Inject for room %q: Sink.Inject requires same-room calls to be serialised", in.Room)
	}
	// A short, deliberate hold so a real overlap (two independent lanes for
	// the same room) has a window to be observed rather than resolving
	// between the CAS above and the CAS below.
	time.Sleep(time.Millisecond)
	atomic.AddInt32(&s.seen, 1)
	if !atomic.CompareAndSwapInt32(&s.inFlight, 1, 0) {
		s.t.Errorf("Inject exited for room %q without holding the guard — instrumentation bug", in.Room)
	}
	return nil
}

func (s *serialGuardSink) Rooms() []string                                  { return nil }
func (s *serialGuardSink) GetAwareness(string) (*awareness.Awareness, bool) { return nil, false }
func (s *serialGuardSink) GetDoc(string) *crdt.Doc                          { return nil }
func (s *serialGuardSink) count() int32                                     { return atomic.LoadInt32(&s.seen) }

// Under Transport: Both, one room must be fed by exactly ONE inbound lane.
//
// This is the reason Transport lives on the existing Config rather than in a
// second relay type: the pub/sub subscriber (runSubscriber) and the stream
// reader (runStreamReader/handleStream) both resolve a room's worker through
// the SAME r.workers map (workerForInbound / workerFor), so both routes feed
// one lane with one consumer goroutine (runRoomWorker) — which is what makes
// same-room Inject calls provably serialised regardless of which tier
// delivered which payload. Two independent lane sets — one populated by
// pub/sub, one by streams — would let this room be Injected concurrently
// from both, violating Sink.Inject's same-room serialisation requirement.
//
// Payloads are genuine V1 update blobs (v1Update, streams_reader_test.go),
// not arbitrary bytes: a non-update payload takes the stream reader's
// catch-up merge-failure fallback (crdt.MergeUpdatesV1 fails, so entries are
// injected individually with a WARN each) instead of the normal merge path,
// which is both avoidable CI log noise and not the path this test means to
// exercise — see streams_reader_test.go:247's v1Update doc for the same
// reasoning applied earlier in this package.
//
// The load-bearing assertion is require.Same on the *roomWorker pointer, not
// len(a.workers): capturing workerForInbound's return at two points spanning
// the whole delivery window pins that no second lane-tracking structure swaps
// room1's worker out from under either tier. The count assertion alone would
// not catch that.
func TestIntegration_StreamLifecycle_BothModeOneLanePerRoom(t *testing.T) {
	mr := newMiniRedis(t)

	cfg := func(node string) Config {
		return Config{
			Transport: Both,
			NodeID:    []byte(node),
			Readers:   1,
			ReadBlock: readerTestBlock,
		}
	}

	sink := &serialGuardSink{t: t}
	a, err := New(newClient(t, mr), cfg(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	b, err := New(newClient(t, mr), cfg(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	a.RoomActivated("room1")
	require.NoError(t, b.Start(ctx, &countingSink{}))

	// The worker RoomActivated just created for room1 — captured here so it
	// can be compared, by pointer identity, against whatever workerForInbound
	// resolves room1 to once both tiers have had a chance to deliver. See the
	// require.Same call below.
	w0, ok := a.workerForInbound("room1")
	require.True(t, ok, "RoomActivated must create room1's worker before either tier can deliver to it")

	// A burst, not a single publish: this widens the window in which a
	// pub/sub-delivered and a stream-delivered copy of these entries could
	// race each other into Inject if they used separate lanes.
	const n = 30
	for i := 0; i < n; i++ {
		require.NoError(t, b.Publish(ctx, cluster.Outbound{
			Room: "room1", Kind: cluster.KindSync, Data: v1Update(t, fmt.Sprintf("msg-%d", i)),
		}))
	}

	require.Eventually(t, func() bool { return sink.count() > 0 },
		5*time.Second, 10*time.Millisecond, "Both mode must deliver at least one entry")
	// Give the stream reader at least one full cycle to also pick up the
	// backlog (it may already have, via pub/sub racing ahead — either way
	// the invariants below must hold once both tiers have had a chance to
	// run).
	time.Sleep(3 * readerTestBlock)

	idx := a.readerFor("room1")
	rooms := a.roomsForReader(idx)
	require.Len(t, rooms, 1, "room1 must be assigned to exactly one reader slot")
	require.Contains(t, rooms, "room1")

	a.workersMu.Lock()
	lanes := len(a.workers)
	a.workersMu.Unlock()
	require.Equal(t, 1, lanes, "one room must have exactly one lane, whichever tier delivered it")

	wLate, ok := a.workerForInbound("room1")
	require.True(t, ok, "room1 must still resolve to a worker after both tiers have delivered")
	require.Same(t, w0, wLate, "the pub/sub subscriber and the stream reader must resolve room1 to the SAME *roomWorker")
}
