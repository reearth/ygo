package websocket

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// idleRecordAdapter records LoadDoc and StoreUpdate call counts so tests can
// assert (a) the durable flush ran on last-peer-leave and (b) a rejoin within
// the idle window reused the warm in-memory doc rather than reloading it.
type idleRecordAdapter struct {
	mu        sync.Mutex
	loadCalls int
	stores    [][]byte
}

func (a *idleRecordAdapter) LoadDoc(string) ([]byte, error) {
	a.mu.Lock()
	a.loadCalls++
	a.mu.Unlock()
	return nil, nil
}

func (a *idleRecordAdapter) StoreUpdate(_ string, u []byte) error {
	a.mu.Lock()
	a.stores = append(a.stores, append([]byte(nil), u...))
	a.mu.Unlock()
	return nil
}

func (a *idleRecordAdapter) loadCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.loadCalls
}

func (a *idleRecordAdapter) storeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.stores)
}

// roomState looks up the room directly (internal test, same package) and
// reports whether it's present plus its idleSince stamp.
func roomState(s *Server, name string) (r *room, present bool, idleSince time.Time) {
	s.rmu.RLock()
	defer s.rmu.RUnlock()
	r, present = s.rooms[name]
	if present {
		r.mu.Lock()
		idleSince = r.idleSince
		r.mu.Unlock()
	}
	return r, present, idleSince
}

// newLastPeerSignal wires OnLastPeer to a buffered channel so tests can wait
// deterministically for the teardown decision (evict vs stamp-idle) to have
// been made, instead of sleeping. Matches the pattern used throughout
// persistence_coalesce_test.go.
func newLastPeerSignal(s *Server) <-chan struct{} {
	ch := make(chan struct{}, 4)
	s.OnLastPeer = func(_ context.Context, _ string) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return ch
}

// With RoomIdleTimeout > 0, the last peer leaving must still perform the
// v1.37.0 durable flush (so data is safe) but must NOT evict: the room stays
// discoverable in s.rooms with idleSince stamped, worker still alive.
func TestIdleRoom_LastPeerLeaveStampsIdleKeepsRoomResident(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	s.RoomIdleTimeout = time.Minute // long enough that no sweeper (T12) matters here
	var unloaded int32
	s.OnUnloadDocument = func(_ context.Context, _ string) {
		atomic.AddInt32(&unloaded, 1)
	}
	lastPeer := newLastPeerSignal(s)
	ts := httptest.NewServer(s)
	defer ts.Close()

	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hi", nil) })
	sendV1Update(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))

	_ = connA.Close()
	<-lastPeer // teardown decision made

	rm, present, idleSince := roomState(s, "room")
	assert.True(t, present, "room must stay resident (not evicted) when RoomIdleTimeout > 0")
	require.NotNil(t, rm)
	assert.False(t, idleSince.IsZero(), "idleSince must be stamped when the room goes idle")
	assert.Equal(t, int32(0), atomic.LoadInt32(&unloaded), "OnUnloadDocument must NOT fire for a resident idle room")
	assert.GreaterOrEqual(t, a.storeCount(), 1, "the durable flush must still happen before stamping idle")

	require.NoError(t, s.Shutdown(context.Background()))
}

// A reconnect within the idle window must reuse the warm in-memory doc: no
// second LoadDoc call, doc content intact, and idleSince cleared.
func TestIdleRoom_RejoinReusesWarmDocNoReload(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	s.RoomIdleTimeout = time.Minute
	lastPeer := newLastPeerSignal(s)
	ts := httptest.NewServer(s)
	defer ts.Close()

	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hello", nil) })
	sendV1Update(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))

	_ = connA.Close()
	<-lastPeer

	loadsBeforeRejoin := a.loadCount()
	assert.Equal(t, 1, loadsBeforeRejoin, "sanity: exactly one LoadDoc for the original room creation")

	docB := crdt.New(crdt.WithClientID(2))
	connB := dialWS(t, ts, "room")
	drainWS(t, connB, docB)

	assert.Equal(t, "hello", docB.GetText("t").ToString(),
		"rejoin must see the edit from the warm in-memory doc")
	assert.Equal(t, loadsBeforeRejoin, a.loadCount(),
		"rejoin within the idle window must NOT call LoadDoc again")

	_, present, idleSince := roomState(s, "room")
	assert.True(t, present)
	assert.True(t, idleSince.IsZero(), "rejoin must clear idleSince")

	require.NoError(t, s.Shutdown(context.Background()))
}

// RoomIdleTimeout == 0 (the default) must preserve exact eager-evict
// behaviour: last peer leaving deletes the room from s.rooms and fires
// OnUnloadDocument, unchanged from pre-#183 behaviour.
func TestIdleRoom_ZeroTimeoutPreservesEagerEvict(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a) // RoomIdleTimeout left at zero value
	var unloaded int32
	s.OnUnloadDocument = func(_ context.Context, _ string) {
		atomic.AddInt32(&unloaded, 1)
	}
	lastPeer := newLastPeerSignal(s)
	ts := httptest.NewServer(s)
	defer ts.Close()

	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hi", nil) })
	sendV1Update(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))

	_ = connA.Close()
	<-lastPeer

	_, present, _ := roomState(s, "room")
	assert.False(t, present, "RoomIdleTimeout=0 must evict the room eagerly, unchanged from prior releases")
	assert.Equal(t, int32(1), atomic.LoadInt32(&unloaded), "OnUnloadDocument must fire on eager eviction")
}
