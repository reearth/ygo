// Package websocket - internal regression tests for issue #182 (G3): room load
// (LoadDoc / decode / OnLoadDocument) must run OFF the global rooms lock
// (s.rmu). A slow or large load of one room must not stall create / lookup /
// evict for every other room. The mechanism is a placeholder room published
// under s.rmu with a `ready chan struct{}`; the load happens off-lock and every
// consumer that touches r.doc first waits on ready.
package websocket

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// blockingLoadAdapter is a PersistenceAdapter whose LoadDoc blocks on a per-room
// gate so a test can hold one room's load open while driving other rooms. It
// also records when each room's LoadDoc is entered and can be told to fail a
// room's load with a specific error.
type blockingLoadAdapter struct {
	mu      sync.Mutex
	gate    map[string]chan struct{} // room → closed to release LoadDoc
	entered map[string]chan struct{} // room → closed when LoadDoc is entered
	loadErr map[string]error         // room → error LoadDoc returns
}

func newBlockingLoadAdapter() *blockingLoadAdapter {
	return &blockingLoadAdapter{
		gate:    map[string]chan struct{}{},
		entered: map[string]chan struct{}{},
		loadErr: map[string]error{},
	}
}

// block marks room so its LoadDoc parks until release(room) is called.
func (a *blockingLoadAdapter) block(room string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gate[room] = make(chan struct{})
	a.entered[room] = make(chan struct{})
}

// setErr makes room's LoadDoc return err (after the gate, if any, releases).
func (a *blockingLoadAdapter) setErr(room string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadErr[room] = err
}

// release unblocks a previously blocked room's LoadDoc.
func (a *blockingLoadAdapter) release(room string) {
	a.mu.Lock()
	g := a.gate[room]
	a.mu.Unlock()
	if g != nil {
		close(g)
	}
}

// enteredCh returns a channel that closes when room's LoadDoc has been entered.
func (a *blockingLoadAdapter) enteredCh(room string) <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entered[room]
}

func (a *blockingLoadAdapter) LoadDoc(room string) ([]byte, error) {
	a.mu.Lock()
	entered := a.entered[room]
	gate := a.gate[room]
	err := a.loadErr[room]
	a.mu.Unlock()
	if entered != nil {
		select {
		case <-entered:
		default:
			close(entered)
		}
	}
	if gate != nil {
		<-gate
	}
	return nil, err
}

func (a *blockingLoadAdapter) StoreUpdate(string, []byte) error { return nil }

// TestGetOrCreateRoom_ConcurrentDistinctRooms_DoNotSerialize is the headline G3
// regression: a blocked load of room A must not stall a create of room B. On the
// pre-fix code the load ran under s.rmu, so B parked on the global lock until A
// unblocked — this test times out there (RED).
func TestGetOrCreateRoom_ConcurrentDistinctRooms_DoNotSerialize(t *testing.T) {
	a := newBlockingLoadAdapter()
	a.block("A") // A's LoadDoc parks until released; B is not gated.
	s := NewServerWithPersistence(a)

	aDone := make(chan struct{})
	go func() {
		_, _ = s.getOrCreateRoom(context.Background(), "A")
		close(aDone)
	}()

	// Wait until A's load is actually in progress so the placeholder is
	// published and (post-fix) s.rmu released.
	select {
	case <-a.enteredCh("A"):
	case <-time.After(2 * time.Second):
		t.Fatal("A's LoadDoc was never entered")
	}

	// B must complete while A is still blocked in LoadDoc.
	bDone := make(chan struct{})
	go func() {
		if _, err := s.getOrCreateRoom(context.Background(), "B"); err != nil {
			t.Errorf("B getOrCreateRoom: %v", err)
		}
		close(bDone)
	}()
	select {
	case <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatal("B serialized behind A's blocked load — room load is still under the global lock (#182)")
	}

	// A must still be parked (sanity: we really did block it).
	select {
	case <-aDone:
		t.Fatal("A returned before it was released")
	default:
	}
	a.release("A")
	<-aDone
}

// TestGetOrCreateRoom_SecondJoinerWaitsOnReady asserts a second caller for the
// same still-loading room parks on the room's ready barrier, not on s.rmu — a
// third caller for a different room proceeds meanwhile. Both same-room callers
// then observe the identical room instance.
func TestGetOrCreateRoom_SecondJoinerWaitsOnReady(t *testing.T) {
	a := newBlockingLoadAdapter()
	a.block("A")
	s := NewServerWithPersistence(a)

	r1done := make(chan *room, 1)
	go func() {
		r, _ := s.getOrCreateRoom(context.Background(), "A")
		r1done <- r
	}()
	select {
	case <-a.enteredCh("A"):
	case <-time.After(2 * time.Second):
		t.Fatal("A's LoadDoc was never entered")
	}

	r2done := make(chan *room, 1)
	go func() {
		r, _ := s.getOrCreateRoom(context.Background(), "A")
		r2done <- r
	}()

	// A different room must still be creatable while the second A joiner waits,
	// proving the joiner does not hold s.rmu.
	if _, err := s.getOrCreateRoom(context.Background(), "C"); err != nil {
		t.Fatalf("C getOrCreateRoom blocked while a joiner waited on ready: %v", err)
	}

	select {
	case <-r1done:
		t.Fatal("first A caller returned before release")
	case <-r2done:
		t.Fatal("second A caller returned before release")
	default:
	}

	a.release("A")
	r1 := <-r1done
	r2 := <-r2done
	if r1 == nil || r1 != r2 {
		t.Fatalf("same-room joiners got different/nil rooms: r1=%p r2=%p", r1, r2)
	}
}

// TestGetOrCreateRoom_LoadErrorWakesWaitersNoPlaceholder asserts a LoadDoc error
// propagates (wrapped) to every waiter and leaves NO placeholder behind in
// s.rooms.
func TestGetOrCreateRoom_LoadErrorWakesWaitersNoPlaceholder(t *testing.T) {
	a := newBlockingLoadAdapter()
	a.block("A")
	wantErr := errors.New("boom-load-failure")
	a.setErr("A", wantErr)
	s := NewServerWithPersistence(a)

	const n = 3
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.getOrCreateRoom(context.Background(), "A")
			errs <- err
		}()
	}

	select {
	case <-a.enteredCh("A"):
	case <-time.After(2 * time.Second):
		t.Fatal("A's LoadDoc was never entered")
	}
	a.release("A")

	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			if err == nil || !errors.Is(err, wantErr) {
				t.Fatalf("waiter got err=%v, want a wrap of %v", err, wantErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a waiter was not woken on load error")
		}
	}

	s.rmu.RLock()
	_, ok := s.rooms["A"]
	s.rmu.RUnlock()
	if ok {
		t.Fatal("failed-load placeholder was left in s.rooms")
	}
	if d := s.GetDoc("A"); d != nil {
		t.Fatal("GetDoc returned non-nil for a failed-load room")
	}
}

// TestGetOrCreateRoom_RelayReentrancyWithPersistence_NoDeadlock exercises the
// hazardous interaction: a relay whose RoomActivated synchronously re-enters
// Sink.Inject (which calls getOrCreateRoom) on a server that also has a
// PersistenceAdapter. RoomActivated must fire only AFTER ready closes, so the
// re-entrant getOrCreateRoom finds the published placeholder and waits on an
// already-closed ready — no deadlock, and the injected update lands.
func TestGetOrCreateRoom_RelayReentrancyWithPersistence_NoDeadlock(t *testing.T) {
	src := crdt.New()
	txt := src.GetText("t")
	src.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })

	a := newBlockingLoadAdapter() // no room gated → loads return immediately
	s := NewServerWithPersistence(a)
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
