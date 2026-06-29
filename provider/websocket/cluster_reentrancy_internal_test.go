// Package websocket - internal regression tests for issue #133: a relay whose
// RoomActivated callback re-enters the Server via Sink.Inject must not deadlock.
package websocket

import (
	"context"
	"testing"
	"time"

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

	runWithDeadlockGuard(t, func() { _, _ = s.getOrCreateRoom(context.Background(), "doc1") })

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

	runWithDeadlockGuard(t, func() { _, _ = s.getOrCreateRoom(context.Background(), "doc1") })

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

	r1, err := s.getOrCreateRoom(context.Background(), "doc1")
	if err != nil {
		t.Fatalf("first getOrCreateRoom: %v", err)
	}
	r2, err := s.getOrCreateRoom(context.Background(), "doc1")
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
