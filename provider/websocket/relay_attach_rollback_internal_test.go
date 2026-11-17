// Package websocket - internal regression test for the attach-rollback race:
// a room whose relay observers are registered against an attach that is
// rolled back mid-registration must not be left subscribed to nothing.
package websocket

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// registerRelayObservers can load its attach state and then be overtaken by
// a failed Start's rollback, whose detachRelayRooms pass runs before the
// observers are stored. Nothing would unsubscribe them afterwards: the room
// is already resident, so a retry never re-registers it, and every later
// edit is enqueued, finds no lane and is counted as a relay drop. The
// registrar must therefore undo its own wiring when the attach it wired to
// has gone.
func TestUnit_RegisterRelayObservers_UndoesItselfWhenItsAttachIsGone(t *testing.T) {
	srv := NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	relay := cluster.NewMemRelay()
	t.Cleanup(func() { require.NoError(t, relay.Close()) })
	require.NoError(t, srv.AttachRelay(relay))

	r, _, err := srv.getOrCreateRoom(context.Background(), "room")
	require.NoError(t, err)
	detachRelayRoom(r) // the rollback's pass over this room, before we store

	// Stand in for a registration that loaded its state and was then
	// overtaken: a state that is no longer the server's.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stale := &relayState{
		relay:    relay,
		sentinel: &relayOriginSentinel{},
		ctx:      ctx,
		cancel:   cancel,
		attached: make(chan struct{}),
	}
	close(stale.attached)
	srv.registerRelayObservers(r, "room", stale)

	r.mu.Lock()
	unsubs, lane := r.relayUnsub, r.relayLane
	r.mu.Unlock()
	require.Nil(t, unsubs, "observers wired to a rolled-back attach were left subscribed")
	require.Nil(t, lane, "a lane was installed for a rolled-back attach")

	// The observers must really be gone, not merely forgotten: an edit that
	// still reaches them is enqueued with no lane to take it, and counted.
	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))
	require.Zero(t, srv.RelayStats().Dropped)
}

// acceptingFailedStartRelay fails its Start but accepts every Publish,
// returning nil without delivering anywhere — what a relay that never
// finished starting does with a payload handed to it.
type acceptingFailedStartRelay struct {
	entered   chan struct{}
	release   chan struct{}
	published chan cluster.Outbound
}

func (r *acceptingFailedStartRelay) Start(context.Context, cluster.Sink) error {
	close(r.entered)
	<-r.release
	return errors.New("relay start failed")
}

func (r *acceptingFailedStartRelay) Publish(_ context.Context, out cluster.Outbound) error {
	select {
	case r.published <- out:
	default:
	}
	return nil
}

func (r *acceptingFailedStartRelay) RoomActivated(string)   {}
func (r *acceptingFailedStartRelay) RoomDeactivated(string) {}
func (r *acceptingFailedStartRelay) Close() error           { return nil }

// A payload held on a lane through the attach window must not be published
// once that attach has been rolled back: the relay never started, so a
// Publish it accepts goes nowhere, and an accepted Publish is not counted —
// the loss would vanish from RelayStats entirely.
//
// Internal because the ordering is what is under test. Holding relayLanesMu
// parks the rollback at its first step, so a lane released before the state
// was cleared has unbounded time to publish: without that the worker loses
// the race every run and the test passes against the defect.
func TestUnit_AttachRelay_FailedStartDoesNotPublishTheHeldBacklog(t *testing.T) {
	relay := &acceptingFailedStartRelay{
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		published: make(chan cluster.Outbound, 8),
	}

	srv := NewServer()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	attached := make(chan error, 1)
	go func() { attached <- srv.AttachRelay(relay) }()
	<-relay.entered // the state is published; Start is parked

	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	// Park the rollback before it can clear the state, then let Start fail.
	// Guarded and registered as a cleanup: the assertion below runs while
	// the lock is held, and a failure there must not leave Shutdown — whose
	// cleanup was registered first, so it runs last — deadlocked on it.
	var once sync.Once
	unlock := func() { once.Do(srv.relayLanesMu.Unlock) }
	srv.relayLanesMu.Lock()
	t.Cleanup(unlock)
	close(relay.release)
	// Long enough for a lane released ahead of the clear to publish.
	time.Sleep(100 * time.Millisecond)
	require.Empty(t, relay.published,
		"the backlog was published to a relay whose Start failed")
	unlock()

	require.Error(t, <-attached)
	require.NoError(t, srv.Shutdown(context.Background()))

	require.Empty(t, relay.published,
		"the backlog was published to a relay whose Start failed")
	require.Equal(t, uint64(1), srv.RelayStats().Dropped,
		"the edit is lost, so it must be counted")
}

// lifecycleRelay records every room activation and deactivation it is
// offered, whether or not it has started.
type lifecycleRelay struct {
	entered     chan struct{}
	release     chan struct{}
	activated   chan string
	deactivated chan string
}

func (r *lifecycleRelay) Start(context.Context, cluster.Sink) error {
	close(r.entered)
	<-r.release
	return nil
}

func (r *lifecycleRelay) Publish(context.Context, cluster.Outbound) error { return nil }

func (r *lifecycleRelay) RoomActivated(room string) {
	select {
	case r.activated <- room:
	default:
	}
}

func (r *lifecycleRelay) RoomDeactivated(room string) {
	select {
	case r.deactivated <- room:
	default:
	}
}

func (r *lifecycleRelay) Close() error { return nil }

// A room torn down while its activation is still held back must take that
// activation with it: the relay was never told the room existed, so telling
// it the room is gone would push its room refcount below where it started,
// and activating it afterwards would subscribe a room nobody is in.
func TestUnit_AttachRelay_TeardownDuringStartTakesTheHeldActivation(t *testing.T) {
	relay := &lifecycleRelay{
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
		activated:   make(chan string, 8),
		deactivated: make(chan string, 8),
	}

	srv := NewServer()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	attached := make(chan error, 1)
	go func() { attached <- srv.AttachRelay(relay) }()
	<-relay.entered

	r, _, err := srv.getOrCreateRoom(context.Background(), "room")
	require.NoError(t, err)
	srv.teardownRelayRoom(r, "room")

	close(relay.release)
	require.NoError(t, <-attached)

	require.Empty(t, relay.deactivated,
		"deactivated a room the relay was never told about")
	require.Empty(t, relay.activated,
		"activated a room that was torn down before the attach finished")
}

// blockingLoadPersistence parks the first LoadDoc until the test releases
// it, holding a room's creation open.
type blockingLoadPersistence struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingLoadPersistence) LoadDoc(string) ([]byte, error) {
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return nil, nil
}

func (p *blockingLoadPersistence) StoreUpdate(string, []byte) error { return nil }

// injectFromStartRelay injects from a goroutine inside Start, then fails:
// the injection is still in flight when the attach is rolled back.
type injectFromStartRelay struct {
	data      []byte
	loading   chan struct{} // closed once the injection is inside the room load
	injected  chan error    // the injection's result, once it finishes
	published chan cluster.Outbound
}

func (r *injectFromStartRelay) Start(ctx context.Context, sink cluster.Sink) error {
	go func() {
		// Start's own context, which the rollback cancels. That does not
		// save the injection: cancellation is only observed after the update
		// has been applied, so it is still stamped and still echoed.
		r.injected <- sink.Inject(ctx, cluster.Inbound{
			Room: "room", Kind: cluster.KindSync, Data: r.data,
		})
	}()
	<-r.loading // the injection is parked in LoadDoc
	return errors.New("relay start failed")
}

func (r *injectFromStartRelay) Publish(_ context.Context, out cluster.Outbound) error {
	select {
	case r.published <- out:
	default:
	}
	return nil
}

func (r *injectFromStartRelay) RoomActivated(string)   {}
func (r *injectFromStartRelay) RoomDeactivated(string) {}
func (r *injectFromStartRelay) Close() error           { return nil }

// An injection stamps its update with the echo-guard origin of the attach
// that delivered it. Loading the room can block, and by the time it resumes
// that attach may have been rolled back and replaced — the room's observers
// then hold the NEW attach's sentinel. Stamping the old one makes the
// observers read a remote update as a local edit and publish it straight
// back to the cluster.
func TestUnit_Inject_StampsTheSentinelTheRoomsObserversHold(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "remote", nil) })

	persistence := &blockingLoadPersistence{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	failing := &injectFromStartRelay{
		data:      crdt.EncodeStateAsUpdateV1(doc, nil),
		loading:   persistence.entered,
		injected:  make(chan error, 1),
		published: make(chan cluster.Outbound, 8),
	}

	srv := NewServerWithPersistence(persistence)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	// The first attach fails with its injection parked mid-load, so the room
	// it creates is wired by the SECOND attach instead.
	require.Error(t, srv.AttachRelay(failing))

	retry := &recordingRelay{published: make(chan cluster.Outbound, 8)}
	require.NoError(t, srv.AttachRelay(retry))

	close(persistence.release)
	// Cancelled by the rollback, but only once the update is already
	// applied — which is exactly why the stamped origin has to be right.
	require.ErrorIs(t, <-failing.injected, context.Canceled)
	require.NoError(t, srv.Shutdown(context.Background()))

	require.Empty(t, retry.published,
		"a remote update was published back to the cluster: it was stamped "+
			"with a sentinel the room's own echo guard does not hold")
	require.Equal(t, "remote", srv.GetDoc("room").GetText("t").ToString(),
		"the update was applied despite the cancellation")
}

// recordingRelay starts cleanly and records what it is asked to publish.
type recordingRelay struct {
	published chan cluster.Outbound
}

func (r *recordingRelay) Start(context.Context, cluster.Sink) error { return nil }

func (r *recordingRelay) Publish(_ context.Context, out cluster.Outbound) error {
	select {
	case r.published <- out:
	default:
	}
	return nil
}

func (r *recordingRelay) RoomActivated(string)   {}
func (r *recordingRelay) RoomDeactivated(string) {}
func (r *recordingRelay) Close() error           { return nil }

// blockingActivationRelay parks inside RoomActivated, naming the room it is
// activating, until the test releases it. It records every lifecycle call.
type blockingActivationRelay struct {
	entered  chan struct{}
	release  chan struct{}
	start    chan struct{} // closed by the test to let Start return
	entering chan string   // the room whose activation is in flight

	mu     sync.Mutex
	events []string
}

func (r *blockingActivationRelay) Start(context.Context, cluster.Sink) error {
	close(r.entered)
	<-r.start
	return nil
}

func (r *blockingActivationRelay) Publish(context.Context, cluster.Outbound) error { return nil }

func (r *blockingActivationRelay) RoomActivated(room string) {
	select {
	case r.entering <- room:
		<-r.release // only the first call parks
	default:
	}
	r.record("activated:" + room)
}

func (r *blockingActivationRelay) RoomDeactivated(room string) {
	r.record("deactivated:" + room)
}

func (r *blockingActivationRelay) record(ev string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *blockingActivationRelay) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *blockingActivationRelay) Close() error { return nil }

// Releasing the held activations is not instantaneous: each RoomActivated is
// a call into the relay, and a room torn down while an earlier one is still
// in flight must not be activated afterwards. Clearing the whole set up
// front loses that: the teardown finds nothing held, issues a
// RoomDeactivated the relay ignores for a room it never activated, and the
// release then subscribes a room that is gone — a subscription nothing will
// ever undo.
func TestUnit_AttachRelay_TeardownDuringActivationReleaseIsNotActivated(t *testing.T) {
	relay := &blockingActivationRelay{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		start:    make(chan struct{}),
		entering: make(chan string, 1),
	}

	srv := NewServer()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	attached := make(chan error, 1)
	go func() { attached <- srv.AttachRelay(relay) }()
	<-relay.entered

	rooms := map[string]*room{}
	for _, name := range []string{"a", "b"} {
		r, _, err := srv.getOrCreateRoom(context.Background(), name)
		require.NoError(t, err)
		rooms[name] = r
	}

	close(relay.start) // Start returns; the release of held activations begins

	// One activation is in flight. Tear the other room down underneath it.
	inFlight := <-relay.entering
	other := "a"
	if inFlight == "a" {
		other = "b"
	}
	srv.teardownRelayRoom(rooms[other], other)

	close(relay.release)
	require.NoError(t, <-attached)

	require.NotContains(t, relay.seen(), "activated:"+other,
		"activated a room torn down while the held activations were being released")
	require.NotContains(t, relay.seen(), "deactivated:"+other,
		"deactivated a room the relay was never told about")
}
