package websocket

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
)

// storedStateAdapter is a PersistenceAdapter whose LoadDoc returns one stored
// state for every room and whose StoreUpdate keeps nothing.
type storedStateAdapter struct{ state []byte }

func (a storedStateAdapter) LoadDoc(string) ([]byte, error)   { return a.state, nil }
func (a storedStateAdapter) StoreUpdate(string, []byte) error { return nil }

// parkedDeletesV1 is a V1 update of no structs whose delete set names n
// one-clock ranges of client, every other clock from 0.
func parkedDeletesV1(client crdt.ClientID, n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // clients
	enc.WriteVarUint(1) // delete set: one client
	enc.WriteVarUint(uint64(client))
	enc.WriteVarUint(uint64(n))
	for i := range n {
		enc.WriteVarUint(uint64(2 * i))
		enc.WriteVarUint(1)
	}
	return append([]byte(nil), enc.Bytes()...)
}

// A room whose stored state holds more parked delete ranges than
// MaxPendingItems loads all the same: the state was written by the room, and
// refusing it would leave a room that never loads. What arrives after the load
// is capped: an honest update applies, and one that parks a further range is
// refused.
func TestUnit_LoadRoom_StoredStateOverThePendingCapLoads(t *testing.T) {
	const limit = 10
	stored := crdt.New(crdt.WithClientID(1), crdt.WithMaxPendingItems(math.MaxInt))
	storedText := stored.GetText("t")
	stored.Transact(func(txn *crdt.Transaction) { storedText.Insert(txn, 0, "hello", nil) })
	if err := crdt.ApplyUpdateV1(stored, parkedDeletesV1(98, limit+5), nil); err != nil {
		t.Fatal(err)
	}
	if got := stored.PendingStats().DeleteRanges; got != limit+5 {
		t.Fatalf("the stored document parks %d delete ranges, want %d", got, limit+5)
	}
	state := crdt.EncodeStateAsUpdateV1(stored, nil)

	if err := crdt.ApplyUpdateV1(crdt.New(crdt.WithMaxPendingItems(limit)), state, nil); !errors.Is(err, crdt.ErrInvalidUpdate) {
		t.Fatalf("a document capped at %d loaded the state with err %v, want ErrInvalidUpdate", limit, err)
	}

	s := NewServerWithPersistence(storedStateAdapter{state: state})
	s.MaxPendingItems = limit
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	r, _, err := s.getOrCreateRoom(context.Background(), "room")
	if err != nil {
		t.Fatalf("the room did not load: %v", err)
	}
	defer s.releaseInflight(r)
	if got := r.doc.GetText("t").ToString(); got != "hello" {
		t.Fatalf("the room's text is %q, want %q", got, "hello")
	}
	if got := r.doc.PendingStats().DeleteRanges; got != limit+5 {
		t.Fatalf("the room parks %d delete ranges, want the stored %d", got, limit+5)
	}

	peer := crdt.New(crdt.WithClientID(2))
	if err := crdt.ApplyUpdateV1(peer, state, nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	peerText := peer.GetText("t")
	peer.Transact(func(txn *crdt.Transaction) {
		peerText.Insert(txn, 5, " world", nil)
		peerText.Delete(txn, 0, 1)
	})
	if err := crdt.ApplyUpdateV1(r.doc, crdt.EncodeStateAsUpdateV1(peer, sv), nil); err != nil {
		t.Fatalf("an honest update after the load was refused: %v", err)
	}
	if got := r.doc.GetText("t").ToString(); got != "ello world" {
		t.Fatalf("the room's text is %q, want %q", got, "ello world")
	}
	if err := crdt.ApplyUpdateV1(r.doc, parkedDeletesV1(97, 1), nil); !errors.Is(err, crdt.ErrInvalidUpdate) {
		t.Fatalf("an update parking a further range was applied with err %v, want ErrInvalidUpdate", err)
	}
	if got := r.doc.PendingStats().DeleteRanges; got != limit+5 {
		t.Fatalf("after the refusal the room parks %d delete ranges, want %d", got, limit+5)
	}
}
