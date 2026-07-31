package websocket_test

import (
	"context"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// stallingRelay blocks Publish for one room until released, standing in for a
// relay that is slow for a single room. Every other room's Publish must still
// get through — the outbound half of #187.
type stallingRelay struct {
	stallRoom string
	release   chan struct{}

	mu        sync.Mutex
	published []cluster.Outbound
}

func newStallingRelay(stallRoom string) *stallingRelay {
	return &stallingRelay{stallRoom: stallRoom, release: make(chan struct{})}
}

func (r *stallingRelay) Publish(ctx context.Context, out cluster.Outbound) error {
	if out.Room == r.stallRoom {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.published = append(r.published, out)
	r.mu.Unlock()
	return nil
}

func (r *stallingRelay) Start(context.Context, cluster.Sink) error { return nil }
func (r *stallingRelay) RoomActivated(string)                      {}
func (r *stallingRelay) RoomDeactivated(string)                    {}
func (r *stallingRelay) Close() error                              { return nil }

func (r *stallingRelay) roomsPublished() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.published))
	for _, p := range r.published {
		out = append(out, p.Room)
	}
	return out
}

// syncUpdate returns one V1 update blob that inserts text into a fresh doc.
func syncUpdate(t *testing.T, s string) []byte {
	t.Helper()
	d := crdt.New()
	txt := d.GetText("t")
	d.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, s, nil) })
	return crdt.EncodeStateAsUpdateV1(d, nil)
}

// applyLocal creates room (dialing a peer connection, exactly like a real
// client joining, so getOrCreateRoom wires the relay observers) and then
// applies update to the room's own doc with a non-sentinel (nil) origin —
// the "documented pattern" also used by inject_test.go's
// TestUnit_BroadcastUpdate_Relays: apply directly to serverDoc, then
// (optionally) fan out.
//
// This is deliberately NOT srv.BroadcastUpdate: BroadcastUpdate (1) requires
// the room to already exist (it looks the room up and returns
// ErrRoomNotFound otherwise) and, more importantly, (2) never touches the
// room's own doc — it only fans already-applied bytes out to already
// -connected peers (see inject.go's broadcastUpdate). It therefore never
// fires doc.OnUpdate and cannot reach registerRelayObservers' relay
// subscription at all, no matter how the outbound path is implemented.
//
// It is also deliberately NOT srv.Apply: this helper predates the fix for
// the zerobase sentinel-collision bug (Apply's origin and AttachRelay's echo
// sentinel used to both be bare `new(struct{})` values, which Go's
// zero-size-allocation guarantee let alias onto the same address —
// see inject.go's applyOriginSentinel and cluster.go's relayOriginSentinel
// doc comments for the full mechanism and TestUnit_Apply_RelaysToOtherNodes
// in inject_test.go for the regression coverage). Kept as a direct
// crdt.ApplyUpdateV1 call rather than switched to srv.Apply now, simply to
// keep this file's helper scoped to what this file's outbound-lane tests
// actually need to exercise.
func applyLocal(t *testing.T, ts *httptest.Server, srv *ygws.Server, room, text string) {
	t.Helper()
	conn := dial(t, ts, room)
	drainHandshake(t, conn, crdt.New())
	require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc(room), syncUpdate(t, text), nil))
}

// THE OUTBOUND #187 GATE: a room whose Publish is wedged must not stop any
// other room from publishing. Fails before the fix, where one worker drains a
// single shared queue.
//
// Strengthened per review: it is not enough to show "fast" eventually
// publishes — that alone would also pass by coincidence if the wedge simply
// wasn't real. So this also asserts "slow" has NOT published while wedged,
// and that releasing the wedge lets its backlog flush through afterwards.
func TestRelayOutbound_CrossRoomIsolation(t *testing.T) {
	relay := newStallingRelay("slow")
	srv := ygws.NewServer()
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		// Do NOT close(relay.release) here: the test body closes it itself
		// once it's done asserting the wedge. If a require.* above fails
		// first, Shutdown's relayCtx cancellation (not this cleanup) is what
		// unwedges the "slow" worker — see stallingRelay.Publish's ctx.Done
		// branch — so a second close here would double-close and panic.
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	// Wedge "slow", then write to "fast".
	applyLocal(t, ts, srv, "slow", "a")
	applyLocal(t, ts, srv, "fast", "b")

	require.Eventually(t, func() bool {
		for _, room := range relay.roomsPublished() {
			if room == "fast" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond,
		`room "fast" must publish while room "slow" is wedged`)

	// The wedge must be real, not a timing fluke that would have let this
	// test pass even without the fix: "slow" must still be parked inside
	// Publish, not have already gone through.
	require.NotContains(t, relay.roomsPublished(), "slow",
		`room "slow" must NOT have published yet — it should still be wedged inside Publish`)

	// Releasing the wedge must let the backlog flush: "slow" publishes too.
	close(relay.release)
	require.Eventually(t, func() bool {
		for _, room := range relay.roomsPublished() {
			if room == "slow" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond,
		`room "slow" must publish once its wedge is released`)
}

// blockingCompactAdapter is a PersistenceAdapter + CompactableAdapter whose
// Compact blocks until released. Compact runs synchronously in the
// persistence worker's exit path on room unload (CompactableAdapter's doc:
// "a slow or hanging Compact delays that room's teardown"), so blocking it
// deterministically holds a room's teardown open at exactly the point this
// test needs: peer.go's handleDisconnect removes the room from Server.rooms
// and closes persistStop BEFORE waiting on persistDone, and persistDone only
// closes after the worker's exit-path Compact call returns. Blocking Compact
// therefore reproduces the window, between "room removed from Server.rooms"
// and "teardownRelayRoom runs", during which a reconnect can create a
// brand-new room instance for the same name — the Critical 1 regression this
// file's TestRelayOutbound_SurvivesEvictionRace covers.
type blockingCompactAdapter struct {
	release     chan struct{}
	blocked     chan struct{}
	once        sync.Once // guards closing blocked, from Compact
	releaseOnce sync.Once // guards closing release, from releaseCompact
}

func newBlockingCompactAdapter() *blockingCompactAdapter {
	return &blockingCompactAdapter{release: make(chan struct{}), blocked: make(chan struct{})}
}

func (a *blockingCompactAdapter) LoadDoc(string) ([]byte, error)   { return nil, nil }
func (a *blockingCompactAdapter) StoreUpdate(string, []byte) error { return nil }
func (a *blockingCompactAdapter) Compact(_ context.Context, _ string) error {
	a.once.Do(func() { close(a.blocked) })
	<-a.release
	return nil
}

// releaseCompact unblocks the Compact call exactly once; safe to call more
// than once (e.g. once from the test body, once unconditionally from
// t.Cleanup) — later calls are no-ops rather than a double-close panic.
//
// t.Cleanup must call this before srv.Shutdown, unconditionally, regardless
// of whether the test body already did. Without it, a require.* failing
// anywhere between newBlockingCompactAdapter and the test body's own
// close(adapter.release) would return via t.FailNow without ever unblocking
// Compact — and Server.Shutdown's persistDone wait only completes after
// Compact returns (CompactableAdapter's doc: it runs synchronously in the
// persistence worker's exit path), so Shutdown(context.Background()) would
// then hang forever on an unbounded context instead of returning promptly, and
// the run would die on go test's own timeout instead of reporting the actual
// assertion failure as a clean FAIL.
func (a *blockingCompactAdapter) releaseCompact() {
	a.releaseOnce.Do(func() { close(a.release) })
}

var (
	_ ygws.PersistenceAdapter = (*blockingCompactAdapter)(nil)
	_ ygws.CompactableAdapter = (*blockingCompactAdapter)(nil)
)

// THE CRITICAL-1 REGRESSION GATE: a room instance recreated while its
// predecessor's teardown is still in flight must keep publishing to the
// relay AFTER that predecessor's teardown completes.
//
// Sequence: peer A joins room "r" (instance A). A disconnects, triggering
// eager eviction: "r" is removed from Server.rooms and persistStop closes,
// but persistDone (and therefore teardownRelayRoom, which retires instance
// A's outbound lane) is held open by the blocked Compact call. While stuck
// there, peer B reconnects for the SAME room name — since Server.rooms has
// no entry, this creates a genuinely new room instance B, with its own
// fresh outbound lane (ensureRelayLane's identity-checked handoff). B
// publishes successfully. Then instance A's teardown is allowed to
// complete: with the pre-fix, name-only stopRelayLane, this would delete
// (and kill the worker for) instance B's live lane, because both instances
// shared the same map key "r". B must still be able to publish after that.
func TestRelayOutbound_SurvivesEvictionRace(t *testing.T) {
	adapter := newBlockingCompactAdapter()
	relay := newStallingRelay("") // "" never matches a real room name: pure capture, no stalling
	srv := ygws.NewServerWithPersistence(adapter)
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		// Unconditionally, before Shutdown: if an assertion below failed
		// early (before the test body's own releaseCompact call), Compact is
		// still blocked, and Shutdown's persistDone wait would otherwise hang
		// forever on it — see releaseCompact's doc.
		adapter.releaseCompact()
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	// A joins "r" (instance A), then leaves immediately — no edits needed;
	// eager eviction (the default) fires purely from the peer count
	// dropping to zero.
	connA := dial(t, ts, "r")
	drainHandshake(t, connA, crdt.New())
	require.NoError(t, connA.Close())

	// Wait until instance A's teardown has reached the blocked Compact call:
	// at this point "r" has already been removed from Server.rooms (that
	// happens strictly before the persistDone wait — see peer.go), but
	// teardownRelayRoom(instance A) has not run yet.
	select {
	case <-adapter.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance A's teardown to reach the blocked Compact call")
	}

	// B reconnects for the same room name while A's teardown is stuck.
	// Server.rooms has no entry for "r" (A already removed it), so this
	// creates a brand-new room instance.
	connB := dial(t, ts, "r")
	drainHandshake(t, connB, crdt.New())
	require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc("r"), syncUpdate(t, "b1"), nil))

	require.Eventually(t, func() bool {
		return len(relay.roomsPublished()) >= 1
	}, 2*time.Second, 10*time.Millisecond,
		"instance B's own outbound lane must publish while instance A's teardown is still stuck")

	// Let instance A's teardown proceed to completion: persistDone closes,
	// then teardownRelayRoom(instance A) retires instance A's lane.
	adapter.releaseCompact()

	// Instance B must keep publishing AFTER instance A's teardown has fully
	// run. Re-applying inside Eventually (rather than a fixed sleep before
	// one check) makes this robust to exactly when A's teardown finishes:
	// each retry both re-drives B's doc and re-checks the publish count, so
	// it converges as soon as the teardown-then-recreate handoff is safe —
	// and never converges (times out) if instance B's lane was wrongly
	// killed by instance A's teardown.
	require.Eventually(t, func() bool {
		_ = crdt.ApplyUpdateV1(srv.GetDoc("r"), syncUpdate(t, "b2"), nil)
		return len(relay.roomsPublished()) >= 2
	}, 2*time.Second, 20*time.Millisecond,
		"instance B's outbound lane must survive instance A's teardown completing (#187 identity-guard regression)")
}

// Outbound relay health must be observable. Before this, relayDropped was
// incremented and read nowhere outside tests, so loss was invisible.
//
// Deviates from the task brief's literal test body in one respect: the brief
// drove the backlog with srv.BroadcastUpdate in a loop with no prior dial.
// That does not compile-fail but always fails at runtime with
// ErrRoomNotFound — BroadcastUpdate requires the room to already exist (see
// its own doc comment) and, per applyLocal's doc comment above (already
// established in this file for exactly this reason), never applies to the
// room's doc or fires doc.OnUpdate, so it cannot reach the outbound relay
// lane at all regardless of RelayStats's implementation. Verified empirically
// during RED: with a stub RelayStats in place, the brief's literal body still
// failed with "ygo/websocket: room not found" on the first loop iteration.
// Using dial+drainHandshake (to create the room and wire relay observers,
// exactly like applyLocal) plus a direct crdt.ApplyUpdateV1 against
// srv.GetDoc (to fire doc.OnUpdate) is the same pattern this file's other
// relay tests already use to reach the outbound path.
func TestRelayStats_Observable(t *testing.T) {
	relay := newStallingRelay("slow")
	srv := ygws.NewServer()
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		close(relay.release)
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	// Join once so the room exists and its relay observers are wired.
	conn := dial(t, ts, "slow")
	drainHandshake(t, conn, crdt.New())

	// Wedge "slow" and pile up a backlog behind it so its lane coalesces.
	for i := 0; i < 200; i++ {
		require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc("slow"), syncUpdate(t, "a"), nil))
	}

	require.Eventually(t, func() bool {
		return srv.RelayStats().Coalesced > 0
	}, 3*time.Second, 20*time.Millisecond,
		"a wedged room's lane must report coalescing")
	require.Zero(t, srv.RelayStats().HardDrops)
}

// RelayStats totals must be monotonic across a room teardown: retiring a
// saturated lane must fold its counters into a running total (see
// stopRelayLane's fold into s.relayRetired) rather than let them vanish from
// the sum. Without that fold, a routine event like this room's last peer
// disconnecting (eager eviction) would make Coalesced silently drop back to
// zero the instant the lane is retired — exactly the kind of decrease that
// defeats a Prometheus-style rate()/increase() read (a decrease reads as a
// counter reset and discards the delta across it). Mirrors
// cluster/redis's TestInteg_Stats_MonotonicAcrossDeactivate.
//
// The sampling loop below asserts more than "no sample decreased": it also
// requires that room "r" actually disappear from Server.Rooms() within the
// window (review fix — an earlier version only polled for a fixed 3s and
// would have passed vacuously, with zero regression signal, if eviction
// never happened inside that window at all, e.g. under some future default
// that delays teardown).
func TestRelayStats_MonotonicAcrossRoomTeardown(t *testing.T) {
	relay := newStallingRelay("r")
	srv := ygws.NewServer()
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		close(relay.release)
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	conn := dial(t, ts, "r")
	drainHandshake(t, conn, crdt.New())

	// Wedge "r" and pile up a backlog so its lane coalesces before teardown.
	for i := 0; i < 200; i++ {
		require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc("r"), syncUpdate(t, "a"), nil))
	}

	var before ygws.RelayStats
	require.Eventually(t, func() bool {
		before = srv.RelayStats()
		return before.Coalesced > 0
	}, 3*time.Second, 20*time.Millisecond,
		"must have coalesced before retiring the room can test anything")

	// Disconnect the room's only peer: eager eviction (the default) fires,
	// which — at some point during this closing sequence — retires "r"'s
	// outbound lane via stopRelayLane/teardownRelayRoom. The lane's worker is
	// still wedged inside Publish (release is not closed until Cleanup), so
	// this exercises stopRelayLane's own fold rather than depending on the
	// worker's final drain ever completing during the test.
	require.NoError(t, conn.Close())

	// Sample repeatedly across the teardown window: every sample must be >=
	// the one before it, and the loop must actually observe "r" disappear
	// from Server.Rooms() — the real eviction event, not merely an elapsed
	// timer — before it is allowed to succeed.
	var evicted bool
	prev := before
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cur := srv.RelayStats()
		require.GreaterOrEqual(t, cur.Coalesced, prev.Coalesced,
			"RelayStats().Coalesced must never decrease, including across the disconnecting room's teardown")
		require.GreaterOrEqual(t, cur.HardDrops, prev.HardDrops)
		prev = cur
		if !slices.Contains(srv.Rooms(), "r") {
			evicted = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.True(t, evicted,
		`room "r" must actually disappear from Server.Rooms() within the window — otherwise this test `+
			"never exercised stopRelayLane's fold at all and its non-decrease checks are vacuous")
	require.GreaterOrEqual(t, prev.Coalesced, before.Coalesced,
		"the coalesced count observed before teardown must still be present in RelayStats after it")

	// stopRelayLane's own fold (see its doc) can lag slightly behind the
	// room's removal from Server.rooms (that removal happens first, in
	// handleDisconnect, strictly before the persistDone wait that gates
	// teardownRelayRoom — see peer.go). Keep sampling briefly past confirmed
	// eviction so this test also observes the fold itself land, not just the
	// room's disappearance.
	require.Eventually(t, func() bool {
		return srv.RelayStats().Coalesced >= before.Coalesced
	}, 1*time.Second, 5*time.Millisecond,
		"RelayStats().Coalesced must reflect stopRelayLane's fold shortly after eviction")
}

// ensureRelayLane's predecessor-displacement handoff is the OTHER lane-
// retirement site (besides stopRelayLane) that must fold a retiring lane's
// counters into s.relayRetired — see ensureRelayLane's doc in cluster.go.
// TestRelayOutbound_SurvivesEvictionRace already drives this exact
// displacement window, but its relay only captures room "" (never stalls
// "r"), so instance A's lane never builds a backlog large enough to
// coalesce there. This test is deliberately separate (rather than bolted
// onto that already-dense test) so it can wedge "r" specifically to force
// coalescing, at the cost of ~15 lines of setup duplicated from both
// TestRelayOutbound_SurvivesEvictionRace (the blockingCompactAdapter
// stuck-teardown technique) and TestRelayStats_MonotonicAcrossRoomTeardown
// (the wedge-and-backlog technique) — judged worth it for a focused,
// unambiguous regression test over retrofitting an existing one.
func TestRelayStats_MonotonicAcrossEnsureRelayLaneHandoff(t *testing.T) {
	adapter := newBlockingCompactAdapter()
	relay := newStallingRelay("r") // wedge "r" itself, unlike SurvivesEvictionRace's ""
	srv := ygws.NewServerWithPersistence(adapter)
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		adapter.releaseCompact()
		close(relay.release)
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	// A joins "r" (instance A) and pushes a backlog large enough to coalesce
	// while "r" is wedged.
	connA := dial(t, ts, "r")
	drainHandshake(t, connA, crdt.New())
	for i := 0; i < 200; i++ {
		require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc("r"), syncUpdate(t, "a"), nil))
	}
	var before ygws.RelayStats
	require.Eventually(t, func() bool {
		before = srv.RelayStats()
		return before.Coalesced > 0
	}, 3*time.Second, 20*time.Millisecond,
		"instance A's lane must coalesce before eviction for this test to prove anything")

	// A disconnects: eager eviction removes "r" from Server.rooms, but
	// teardownRelayRoom(instance A) — which would otherwise retire instance
	// A's lane via stopRelayLane — is held open by the blocked Compact call.
	require.NoError(t, connA.Close())
	select {
	case <-adapter.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instance A's teardown to reach the blocked Compact call")
	}

	// B reconnects for the same room name while A's teardown is still stuck.
	// Server.rooms has no entry for "r" (A already removed it), so this
	// creates instance B and calls ensureRelayLane, which finds instance A's
	// lane still sitting in s.relayLanes["r"] and must displace it — folding
	// its Coalesced count into s.relayRetired itself, since stopRelayLane
	// will never run for instance A's lane (ensureRelayLane's handoff beats
	// it to the punch — see ensureRelayLane's doc).
	connB := dial(t, ts, "r")
	drainHandshake(t, connB, crdt.New())

	require.Eventually(t, func() bool {
		return srv.RelayStats().Coalesced >= before.Coalesced
	}, 2*time.Second, 10*time.Millisecond,
		"RelayStats().Coalesced must not fall across ensureRelayLane's predecessor-displacement handoff")

	// Let instance A's stuck teardown finish so t.Cleanup's Shutdown isn't
	// racing it unnecessarily.
	adapter.releaseCompact()
}
