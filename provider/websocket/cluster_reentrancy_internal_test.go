// Package websocket - internal regression tests for issue #133: a relay whose
// RoomActivated callback re-enters the Server via Sink.Inject must not deadlock.
package websocket

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// reentrantActivationRelay reproduces issue #133: a cluster.Relay whose
// RoomActivated synchronously delivers caught-up history by calling Sink.Inject.
// Inject re-enters getOrCreateRoom; if RoomActivated runs while the Server still
// holds s.rmu, the non-reentrant mutex self-deadlocks the whole instance. This is
// exactly the cross-instance path: a second node activating an already-active
// room replays existing stream entries during activation.
type reentrantActivationRelay struct {
	sink      cluster.Sink
	kind      cluster.Kind
	data      []byte
	injectErr error // written in RoomActivated, read after the activation returns
}

func (r *reentrantActivationRelay) Publish(context.Context, cluster.Outbound) error { return nil }

func (r *reentrantActivationRelay) Start(_ context.Context, sink cluster.Sink) error {
	r.sink = sink
	return nil
}

func (r *reentrantActivationRelay) RoomActivated(room string) {
	// Catch-up replay delivered synchronously from inside the activation
	// callback — the natural way for a relay to push stream history.
	r.injectErr = r.sink.Inject(context.Background(), cluster.Inbound{
		Room: room, Kind: r.kind, Data: r.data,
	})
}

func (r *reentrantActivationRelay) RoomDeactivated(string) {}
func (r *reentrantActivationRelay) Close() error           { return nil }

// runWithDeadlockGuard runs fn in a goroutine and fails the test if it does not
// return within 2s — a deadlocked getOrCreateRoom parks forever on s.rmu, so a
// timeout is the only way to observe the bug without hanging the whole binary.
func runWithDeadlockGuard(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("getOrCreateRoom deadlocked: RoomActivated re-entered Sink.Inject " +
			"while the Server held s.rmu (#133)")
	}
}

// TestGetOrCreateRoom_ReentrantSyncInjectFromRoomActivated asserts that a relay
// which replays a document update from within RoomActivated does not deadlock,
// and that the replayed update reaches the room.
func TestGetOrCreateRoom_ReentrantSyncInjectFromRoomActivated(t *testing.T) {
	// A valid V1 update for the activation replay to inject. The shared-type ref
	// is resolved OUTSIDE the Transact closure (GetText inside Transact deadlocks).
	src := crdt.New()
	txt := src.GetText("t")
	src.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })

	s := NewServer()
	relay := &reentrantActivationRelay{kind: cluster.KindSync, data: crdt.EncodeStateAsUpdateV1(src, nil)}
	if err := s.AttachRelay(relay); err != nil {
		t.Fatalf("AttachRelay: %v", err)
	}

	runWithDeadlockGuard(t, func() { _, _, _ = s.getOrCreateRoom(context.Background(), "doc1") })

	if relay.injectErr != nil {
		t.Fatalf("re-entrant Inject returned error: %v", relay.injectErr)
	}
	d := s.GetDoc("doc1")
	if d == nil {
		t.Fatal("room doc1 was not created")
	}
	if got := d.GetText("t").ToString(); got != "hi" {
		t.Fatalf("injected update not applied: GetText = %q, want %q", got, "hi")
	}
}

// TestGetOrCreateRoom_ReentrantAwarenessInjectFromRoomActivated covers the other
// Inject branch (KindAwareness), which also re-enters getOrCreateRoom.
func TestGetOrCreateRoom_ReentrantAwarenessInjectFromRoomActivated(t *testing.T) {
	s := NewServer()
	relay := &reentrantActivationRelay{kind: cluster.KindAwareness, data: awarenessUpdateFor(7)}
	if err := s.AttachRelay(relay); err != nil {
		t.Fatalf("AttachRelay: %v", err)
	}

	runWithDeadlockGuard(t, func() { _, _, _ = s.getOrCreateRoom(context.Background(), "doc1") })

	if relay.injectErr != nil {
		t.Fatalf("re-entrant Inject returned error: %v", relay.injectErr)
	}
	if aw, ok := s.GetAwareness("doc1"); !ok || aw == nil {
		t.Fatal("awareness for doc1 was not created")
	}
}

// roomActivatedProbeRelay records each RoomActivated call and whether the room
// was already published into s.rooms (GetDoc != nil) at callback time.
type roomActivatedProbeRelay struct {
	sink                cluster.Sink
	activations         []string
	publishedAtCallback []bool
}

func (r *roomActivatedProbeRelay) Publish(context.Context, cluster.Outbound) error { return nil }
func (r *roomActivatedProbeRelay) Start(_ context.Context, sink cluster.Sink) error {
	r.sink = sink
	return nil
}
func (r *roomActivatedProbeRelay) RoomActivated(room string) {
	r.activations = append(r.activations, room)
	r.publishedAtCallback = append(r.publishedAtCallback, r.sink.GetDoc(room) != nil)
}
func (r *roomActivatedProbeRelay) RoomDeactivated(string) {}
func (r *roomActivatedProbeRelay) Close() error           { return nil }

// TestGetOrCreateRoom_RoomActivatedFiresOnceOffLockAfterPublish asserts the fix's
// invariant: RoomActivated fires exactly once per created room, after the room is
// published into s.rooms and after s.rmu is released (so a re-entrant Inject can
// find the room). A second getOrCreateRoom for the same name must not re-fire it.
func TestGetOrCreateRoom_RoomActivatedFiresOnceOffLockAfterPublish(t *testing.T) {
	s := NewServer()
	relay := &roomActivatedProbeRelay{}
	if err := s.AttachRelay(relay); err != nil {
		t.Fatalf("AttachRelay: %v", err)
	}

	r1, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	if err != nil {
		t.Fatalf("first getOrCreateRoom: %v", err)
	}
	r2, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	if err != nil {
		t.Fatalf("second getOrCreateRoom: %v", err)
	}
	if r1 != r2 {
		t.Fatal("getOrCreateRoom returned a different room on the second call")
	}

	if len(relay.activations) != 1 {
		t.Fatalf("RoomActivated fired %d times, want exactly 1", len(relay.activations))
	}
	if relay.activations[0] != "doc1" {
		t.Fatalf("RoomActivated room = %q, want %q", relay.activations[0], "doc1")
	}
	if !relay.publishedAtCallback[0] {
		t.Fatal("RoomActivated fired before the room was published into s.rooms (#133)")
	}
}

// captureRelay records every Publish call so a test can inspect the outbound
// events the relay observers produced. Unlike reentrantActivationRelay it does
// not drive any Inject re-entrancy; it is a plain capture sink.
type captureRelay struct {
	mu   sync.Mutex
	outs []cluster.Outbound
}

func (r *captureRelay) Publish(_ context.Context, out cluster.Outbound) error {
	r.mu.Lock()
	r.outs = append(r.outs, out)
	r.mu.Unlock()
	return nil
}

func (r *captureRelay) Start(context.Context, cluster.Sink) error { return nil }
func (r *captureRelay) RoomActivated(string)                      {}
func (r *captureRelay) RoomDeactivated(string)                    {}
func (r *captureRelay) Close() error                              { return nil }

// awarenessEvents returns a snapshot of the captured KindAwareness events.
func (r *captureRelay) awarenessEvents() []cluster.Outbound {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]cluster.Outbound, 0, len(r.outs))
	for _, ob := range r.outs {
		if ob.Kind == cluster.KindAwareness {
			out = append(out, ob)
		}
	}
	return out
}

// waitForAwarenessEvents polls until at least n KindAwareness events have been
// captured, or fails the test after a timeout. The relay worker drains the
// outbound queue on its own goroutine (enqueueRelayOutbound is non-blocking),
// so Publish is asynchronous with respect to the ApplyUpdate call that
// triggers it.
func waitForAwarenessEvents(t *testing.T, r *captureRelay, n int) []cluster.Outbound {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if evs := r.awarenessEvents(); len(evs) >= n {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d KindAwareness relay events, got %d", n, len(r.awarenessEvents()))
	return nil
}

// TestRegisterRelayObservers_HeartbeatPropagatesToRelay is the regression test
// for the #105/#175 review finding: registerRelayObservers used to subscribe
// the awareness side via OnChange, which does NOT fire for a content-identical
// heartbeat re-emit (only the clock advances). In a cluster deployment this
// silently dropped heartbeats from cross-node relay, so a remote node's cached
// awareness meta went stale and could falsely expire a still-alive client. The
// fix subscribes via OnUpdate instead, which fires on every applied entry
// including heartbeats. This test proves a heartbeat now reaches the relay.
func TestRegisterRelayObservers_HeartbeatPropagatesToRelay(t *testing.T) {
	relay := &captureRelay{}
	s := NewServer()
	if err := s.AttachRelay(relay); err != nil {
		t.Fatalf("AttachRelay: %v", err)
	}

	rm, _, err := s.getOrCreateRoom(context.Background(), "room")
	if err != nil {
		t.Fatalf("getOrCreateRoom: %v", err)
	}

	// Establish an active remote awareness client (id 55) in the room's
	// awareness, as if a peer on another node had announced itself. Applying
	// with a non-sentinel origin makes it look like a locally-originated
	// change from the relay observer's point of view, so it is eligible for
	// relay (matching how a locally-connected peer's awareness update arrives
	// at the room's awareness via peer.go).
	const remoteID = uint64(55)
	remoteAw := awareness.New(remoteID)
	remoteAw.SetLocalState(map[string]any{"cursor": 1})
	initialUpdate := remoteAw.EncodeUpdate([]uint64{remoteID})
	if err := rm.awareness.ApplyUpdate(initialUpdate, "peer-origin"); err != nil {
		t.Fatalf("ApplyUpdate (initial): %v", err)
	}

	// The initial (content-changing) update must have relayed — sanity check
	// that the relay path is wired at all before we test the heartbeat case.
	waitForAwarenessEvents(t, relay, 1)

	// Now apply a HEARTBEAT from that same client: a content-identical
	// re-emit at a bumped clock. Heartbeat() only bumps remoteAw's own clock
	// (it does not touch the room's awareness), so EncodeUpdate(nil) after it
	// carries the same state JSON at a higher clock — exactly what a remote
	// node's periodic liveness re-emit looks like on the wire.
	remoteAw.Heartbeat()
	heartbeatUpdate := remoteAw.EncodeUpdate(nil)
	if err := rm.awareness.ApplyUpdate(heartbeatUpdate, "peer-origin"); err != nil {
		t.Fatalf("ApplyUpdate (heartbeat): %v", err)
	}

	// Before the fix, this second event would never arrive: OnChange does not
	// fire for a content-identical, clock-only-bumped update, so the relay
	// observer built on OnChange never saw the heartbeat.
	evs := waitForAwarenessEvents(t, relay, 2)
	if len(evs) != 2 {
		t.Fatalf("got %d KindAwareness relay events after heartbeat, want exactly 2 (initial + heartbeat)", len(evs))
	}
}

// TestRegisterRelayObservers_SentinelEchoStillDropped is a narrow companion
// check: the echo guard on the OnUpdate-based awareness relay subscription
// must still drop updates that arrive with the relay's own sentinel origin,
// exactly as it did on the OnChange-based subscription before this fix.
func TestRegisterRelayObservers_SentinelEchoStillDropped(t *testing.T) {
	relay := &captureRelay{}
	s := NewServer()
	if err := s.AttachRelay(relay); err != nil {
		t.Fatalf("AttachRelay: %v", err)
	}

	// Inject (as the relay itself would) using the server's sentinel origin.
	// Inject auto-creates the room.
	if err := s.Inject(context.Background(), cluster.Inbound{
		Room: "room", Kind: cluster.KindAwareness, Data: awarenessUpdateFor(77),
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// Give the relay worker a moment to have processed anything that
	// might have been enqueued, then assert nothing was published: the inbound
	// merge used the sentinel origin, so the echo guard must have dropped it.
	time.Sleep(50 * time.Millisecond)
	if evs := relay.awarenessEvents(); len(evs) != 0 {
		t.Fatalf("sentinel-origin inbound merge must not be re-published to the relay; got %d events", len(evs))
	}
}
