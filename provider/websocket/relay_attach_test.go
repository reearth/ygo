// Regression tests for the AttachRelay attach handshake: a relay that
// delivers traffic while AttachRelay is still running must find a server
// that is already wired, and a relay whose Start fails must leave the
// server unattached and retryable.
package websocket_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// injectDuringStartRelay delivers one remote update from inside Start —
// the window AttachRelay is still running. A relay attached to a bus other
// nodes already publish on does exactly this.
type injectDuringStartRelay struct {
	room      string
	data      []byte
	startErr  error
	injectErr error // written in Start, read after AttachRelay returns
	published chan cluster.Outbound
}

func (r *injectDuringStartRelay) Start(ctx context.Context, sink cluster.Sink) error {
	r.injectErr = sink.Inject(ctx, cluster.Inbound{
		Room: r.room, Kind: cluster.KindSync, Data: r.data,
	})
	return r.startErr
}

func (r *injectDuringStartRelay) Publish(_ context.Context, out cluster.Outbound) error {
	select {
	case r.published <- out:
	default:
	}
	return nil
}

func (r *injectDuringStartRelay) RoomActivated(string)   {}
func (r *injectDuringStartRelay) RoomDeactivated(string) {}
func (r *injectDuringStartRelay) Close() error           { return nil }

// remoteUpdate is a V1 update inserting text into "t" of a foreign doc.
func remoteUpdate(t *testing.T, clientID crdt.ClientID, s string) []byte {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(clientID))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, s, nil) })
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// A room the relay brings into existence during Start must come up fully
// relay-wired: its local changes publish, and the update that created it
// carries the relay sentinel, so the echo guard drops it.
func TestUnit_AttachRelay_RoomCreatedDuringStartIsWired(t *testing.T) {
	relay := &injectDuringStartRelay{
		room:      "room",
		data:      remoteUpdate(t, 1, "remote"),
		published: make(chan cluster.Outbound, 8),
	}

	srv := ygws.NewServer()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	require.NoError(t, srv.AttachRelay(relay))
	require.NoError(t, relay.injectErr)

	// The injected update landed in a room created inside the attach window.
	doc := srv.GetDoc("room")
	require.NotNil(t, doc)
	require.Equal(t, "remote", doc.GetText("t").ToString())

	// That room must be relay-wired: a local edit publishes to the relay.
	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	// Shutdown retires the lanes, drains them and joins their workers, so
	// once it returns every publish this server will ever make has been
	// made — no sleep needed, and no late one can arrive mid-assertion.
	require.NoError(t, srv.Shutdown(context.Background()))

	var published []cluster.Outbound
	for len(relay.published) > 0 {
		published = append(published, <-relay.published)
	}
	// Exactly one: the local edit. The update injected during Start carried
	// the relay sentinel, so the echo guard dropped it.
	require.Len(t, published, 1, "expected only the local edit to publish; "+
		"none means the room the relay created during AttachRelay was never "+
		"wired, two means the injected update was echoed back")

	out := published[0]
	assert.Equal(t, cluster.KindSync, out.Kind)
	assert.Equal(t, "room", out.Room)

	// The payload is that edit, not just any sync frame: replayed onto the
	// state the relay injected, it yields the edited text.
	replay := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(replay, relay.data, nil))
	require.NoError(t, crdt.ApplyUpdateV1(replay, out.Data, nil))
	assert.Equal(t, "localremote", replay.GetText("t").ToString())
}

// A Start failure must leave the server unattached, so the call can be
// retried with another relay.
func TestUnit_AttachRelay_FailedStartLeavesServerUnattached(t *testing.T) {
	boom := errors.New("relay start failed")
	failing := &injectDuringStartRelay{
		room:      "room",
		data:      remoteUpdate(t, 1, "remote"),
		startErr:  boom,
		published: make(chan cluster.Outbound, 8),
	}

	// Cleanups run LIFO, so the relay registered first is closed last: the
	// server shuts its lanes down while the relay is still open.
	retry := cluster.NewMemRelay()
	t.Cleanup(func() { require.NoError(t, retry.Close()) })
	srv := ygws.NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	require.ErrorIs(t, srv.AttachRelay(failing), boom)

	// Unattached: a retry with a different relay is accepted, not refused
	// with ErrRelayAlreadyAttached.
	require.NoError(t, srv.AttachRelay(retry))
}

// The rollback must also unwire the rooms the failed attempt created.
// Leaving their observers subscribed would feed a retired lane: every later
// local edit in that room is enqueued, finds no lane and is counted as a
// relay drop, for as long as the room stays resident.
func TestUnit_AttachRelay_FailedStartUnwiresTheRoomsItCreated(t *testing.T) {
	boom := errors.New("relay start failed")
	failing := &injectDuringStartRelay{
		room:      "room",
		data:      remoteUpdate(t, 1, "remote"),
		startErr:  boom,
		published: make(chan cluster.Outbound, 8),
	}

	retry := cluster.NewMemRelay()
	t.Cleanup(func() { require.NoError(t, retry.Close()) })
	srv := ygws.NewServer()
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	require.ErrorIs(t, srv.AttachRelay(failing), boom)
	require.NotNil(t, srv.GetDoc("room"), "the failed attempt created the room")
	require.NoError(t, srv.AttachRelay(retry))

	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	assert.Zero(t, srv.RelayStats().Dropped,
		"a room left wired to the rolled-back attach drops its edits")
}

// rejectUntilStartedRelay refuses to publish until its Start has returned —
// what any relay that gates Publish on its own startup does, cluster/redis
// included. Start blocks until release is closed, so the test owns the whole
// attach window.
type rejectUntilStartedRelay struct {
	entered   chan struct{} // closed on entry to Start: the state is published
	release   chan struct{} // closed by the test to let Start return
	started   atomic.Bool
	attempts  atomic.Uint64 // Publish calls, accepted or not
	published chan cluster.Outbound
}

func (r *rejectUntilStartedRelay) Start(context.Context, cluster.Sink) error {
	close(r.entered)
	<-r.release
	r.started.Store(true)
	return nil
}

func (r *rejectUntilStartedRelay) Publish(_ context.Context, out cluster.Outbound) error {
	r.attempts.Add(1)
	if !r.started.Load() {
		return errors.New("relay not started")
	}
	select {
	case r.published <- out:
	default:
	}
	return nil
}

func (r *rejectUntilStartedRelay) RoomActivated(string)   {}
func (r *rejectUntilStartedRelay) RoomDeactivated(string) {}
func (r *rejectUntilStartedRelay) Close() error           { return nil }

// An edit made while AttachRelay is still inside Start must not be published
// to a relay that has not started: the payload is already off the lane, so a
// rejected Publish loses it for good and the other nodes never see the edit.
// The lane holds it until the attach finishes instead.
func TestUnit_AttachRelay_EditDuringStartIsNotPublishedBeforeTheRelayStarts(t *testing.T) {
	relay := &rejectUntilStartedRelay{
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		published: make(chan cluster.Outbound, 8),
	}

	srv := ygws.NewServer()
	attached := make(chan error, 1)
	go func() { attached <- srv.AttachRelay(relay) }()

	// Start has been entered, so the attach state is published and rooms
	// created from here on get a live outbound lane.
	<-relay.entered

	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	// The lane worker has the payload. It must not hand it to a relay whose
	// Start has not returned.
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, relay.attempts.Load(),
		"the edit was published before the relay started; a rejection there loses it")

	close(relay.release)
	require.NoError(t, <-attached)
	require.NoError(t, srv.Shutdown(context.Background()))

	assert.Zero(t, srv.RelayStats().Dropped, "the edit was dropped")
	require.Len(t, relay.published, 1, "the edit never reached the relay")
}

// activationTrackingRelay refuses room activations until its Start has
// returned, as a relay that subscribes per room must: there is nothing to
// subscribe with yet. Start blocks until the test releases it.
type activationTrackingRelay struct {
	entered   chan struct{}
	release   chan struct{}
	started   atomic.Bool
	activated chan string
	refused   atomic.Uint64
}

func (r *activationTrackingRelay) Start(context.Context, cluster.Sink) error {
	close(r.entered)
	<-r.release
	r.started.Store(true)
	return nil
}

func (r *activationTrackingRelay) Publish(context.Context, cluster.Outbound) error {
	return nil
}

func (r *activationTrackingRelay) RoomActivated(room string) {
	if !r.started.Load() {
		r.refused.Add(1) // nothing records it; the room is never subscribed
		return
	}
	select {
	case r.activated <- room:
	default:
	}
}

func (r *activationTrackingRelay) RoomDeactivated(string) {}
func (r *activationTrackingRelay) Close() error           { return nil }

// A room created while AttachRelay is still inside Start must still end up
// activated. The relay cannot accept an activation before it has started,
// and nothing repeats the call — getOrCreateRoom issues exactly one per
// room — so an activation made in that window is lost and the room is never
// subscribed: remote edits never reach it for as long as it stays resident.
func TestUnit_AttachRelay_RoomCreatedDuringStartIsActivatedAfterIt(t *testing.T) {
	relay := &activationTrackingRelay{
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		activated: make(chan string, 8),
	}

	srv := ygws.NewServer()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	attached := make(chan error, 1)
	go func() { attached <- srv.AttachRelay(relay) }()
	<-relay.entered

	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	close(relay.release)
	require.NoError(t, <-attached)

	assert.Zero(t, relay.refused.Load(),
		"the activation was offered before the relay could accept it")
	select {
	case room := <-relay.activated:
		assert.Equal(t, "room", room)
	default:
		t.Fatal("the room created during the attach was never activated")
	}
}

// blockingStartRelay runs for as long as its Start context lives — a relay
// whose Start is its own run loop. AttachRelay therefore does not return
// until the server is shut down.
type blockingStartRelay struct {
	entered chan struct{}
}

func (r *blockingStartRelay) Start(ctx context.Context, _ cluster.Sink) error {
	close(r.entered)
	<-ctx.Done()
	return ctx.Err()
}

func (r *blockingStartRelay) Publish(context.Context, cluster.Outbound) error { return nil }
func (r *blockingStartRelay) RoomActivated(string)                            {}
func (r *blockingStartRelay) RoomDeactivated(string)                          {}
func (r *blockingStartRelay) Close() error                                    { return nil }

// Shutdown must finish even if the attach never did. A lane holding a
// payload through the attach window waits for a gate only AttachRelay can
// close, so joining those workers before cancelling the relay context waits
// for a relay that is itself waiting for that cancel — and with no deadline
// on the Shutdown context, neither side ever moves.
func TestUnit_Shutdown_CompletesWhileAnAttachIsStillRunning(t *testing.T) {
	relay := &blockingStartRelay{entered: make(chan struct{})}

	srv := ygws.NewServer()
	attached := make(chan error, 1)
	go func() { attached <- srv.AttachRelay(relay) }()
	<-relay.entered

	// A payload on a lane, parked waiting for the attach to finish.
	require.NoError(t, srv.Apply(context.Background(), "room",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "local", nil) })
		}))

	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned: it joined lane workers that were " +
			"waiting for an attach only its own cancel can end")
	}

	select {
	case <-attached:
	case <-time.After(5 * time.Second):
		t.Fatal("AttachRelay never returned after Shutdown cancelled the relay context")
	}
}
