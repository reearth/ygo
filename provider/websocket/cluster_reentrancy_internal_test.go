// Package websocket - internal regression test for issue #133.
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
	sink   cluster.Sink
	update []byte
}

func (r *reentrantActivationRelay) Publish(context.Context, cluster.Outbound) error { return nil }

func (r *reentrantActivationRelay) Start(_ context.Context, sink cluster.Sink) error {
	r.sink = sink
	return nil
}

func (r *reentrantActivationRelay) RoomActivated(room string) {
	// Catch-up replay delivered synchronously from inside the activation
	// callback — the natural way for a relay to push stream history.
	_ = r.sink.Inject(context.Background(), cluster.Inbound{
		Room: room, Kind: cluster.KindSync, Data: r.update,
	})
}

func (r *reentrantActivationRelay) RoomDeactivated(string) {}
func (r *reentrantActivationRelay) Close() error           { return nil }

// TestGetOrCreateRoom_ReentrantInjectFromRoomActivated_NoDeadlock asserts that a
// relay which calls Sink.Inject from within RoomActivated does not deadlock the
// Server, and that the replayed update reaches the room (#133).
func TestGetOrCreateRoom_ReentrantInjectFromRoomActivated_NoDeadlock(t *testing.T) {
	// A valid V1 update for the activation replay to inject. The shared-type ref
	// is resolved OUTSIDE the Transact closure (GetText inside Transact deadlocks).
	src := crdt.New()
	txt := src.GetText("t")
	src.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })
	update := crdt.EncodeStateAsUpdateV1(src, nil)

	s := NewServer()
	relay := &reentrantActivationRelay{update: update}
	if err := s.AttachRelay(relay); err != nil {
		t.Fatalf("AttachRelay: %v", err)
	}

	done := make(chan struct{})
	go func() {
		// getOrCreateRoom fires RoomActivated, which re-enters via Sink.Inject.
		_, _ = s.getOrCreateRoom(context.Background(), "doc1")
		close(done)
	}()

	select {
	case <-done:
		// reached: no deadlock.
	case <-time.After(2 * time.Second):
		t.Fatal("getOrCreateRoom deadlocked: RoomActivated re-entered Sink.Inject " +
			"while the Server held s.rmu (#133)")
	}

	// The off-lock activation replay must have materialised the room and applied
	// the injected update.
	d := s.GetDoc("doc1")
	if d == nil {
		t.Fatal("room doc1 was not created")
	}
	if got := d.GetText("t").ToString(); got != "hi" {
		t.Fatalf("injected update not applied: GetText = %q, want %q", got, "hi")
	}
}
