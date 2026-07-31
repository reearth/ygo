package websocket_test

import (
	"context"
	"net/http/httptest"
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
// It is also deliberately NOT srv.Apply: Apply tags its own transaction with
// a private `origin := new(struct{})` sentinel, and Go's zero-size-value
// guarantee (https://go.dev/ref/spec#Size_and_alignment_guarantees) means
// EVERY `new(struct{})` in the process may share one address — in practice
// Apply's origin pointer compares equal (==) to AttachRelay's own
// `relaySentinel := new(struct{})`. That makes registerRelayObservers' echo
// guard (`if origin == sentinel`) always fire for an Apply-driven change,
// silently swallowing it before it ever reaches enqueueRelayOutbound. This
// is a pre-existing latent bug in Server.Apply + relay interaction, entirely
// outside Task 5's file scope (inject.go, not cluster.go/server.go) — flagged
// separately rather than fixed here.
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
	release chan struct{}
	blocked chan struct{}
	once    sync.Once
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
	close(adapter.release)

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
