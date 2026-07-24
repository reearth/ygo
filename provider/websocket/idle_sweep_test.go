package websocket

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUnloadSignal wires OnUnloadDocument to a buffered per-room channel so a
// test can wait deterministically for a specific room's eviction (rather than
// sleeping). Returns a func that blocks until the named room has been unloaded
// or the deadline fires.
func newUnloadSignal(s *Server) (waitUnloaded func(t *testing.T, name string, d time.Duration) bool) {
	var mu sync.Mutex
	chans := map[string]chan struct{}{}
	get := func(name string) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		ch, ok := chans[name]
		if !ok {
			ch = make(chan struct{}, 1)
			chans[name] = ch
		}
		return ch
	}
	s.OnUnloadDocument = func(_ context.Context, name string) {
		select {
		case get(name) <- struct{}{}:
		default:
		}
	}
	return func(t *testing.T, name string, d time.Duration) bool {
		t.Helper()
		select {
		case <-get(name):
			return true
		case <-time.After(d):
			return false
		}
	}
}

// The background sweeper must evict a room whose idle time exceeds
// RoomIdleTimeout: OnUnloadDocument fires, the room is gone from s.rooms, and
// the durable flush ran before eviction.
func TestIdleSweep_EvictsRoomAfterTimeout(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	s.RoomIdleTimeout = 30 * time.Millisecond
	s.idleSweepInterval = 5 * time.Millisecond
	waitUnloaded := newUnloadSignal(s)
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
	<-lastPeer // room stamped idle

	// Sanity: the durable flush ran before the room could be swept.
	require.GreaterOrEqual(t, a.storeCount(), 1, "durable flush must run before eviction")

	require.True(t, waitUnloaded(t, "room", 2*time.Second),
		"sweeper must fire OnUnloadDocument for a room idle past RoomIdleTimeout")

	_, present, _ := roomState(s, "room")
	assert.False(t, present, "swept room must be gone from s.rooms")
	require.True(t, s.sweeperStarted.Load(), "sweeper goroutine must have started")

	require.NoError(t, s.Shutdown(context.Background()))
}

// The sweeper must NEVER evict a room that has a live peer, even when a stale
// idle stamp is present: it rechecks len(peers)==0, not idleSince alone. Here a
// peer rejoins within the idle window (which clears idleSince); the room must
// survive well past RoomIdleTimeout.
func TestIdleSweep_DoesNotEvictRejoinedRoom(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	s.RoomIdleTimeout = 30 * time.Millisecond
	s.idleSweepInterval = 5 * time.Millisecond
	waitUnloaded := newUnloadSignal(s)
	lastPeer := newLastPeerSignal(s)
	ts := httptest.NewServer(s)
	defer ts.Close()

	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)
	_ = connA.Close()
	<-lastPeer // stamped idle

	// Rejoin before the sweeper reaps it; registration clears idleSince.
	docB := crdt.New(crdt.WithClientID(2))
	connB := dialWS(t, ts, "room")
	drainWS(t, connB, docB)

	// Wait well past several idle-timeout windows: an occupied room must survive.
	assert.False(t, waitUnloaded(t, "room", 300*time.Millisecond),
		"sweeper must NOT evict a room with a live peer")

	rm, present, idleSince := roomState(s, "room")
	require.True(t, present, "rejoined room must stay resident")
	require.NotNil(t, rm)
	assert.True(t, idleSince.IsZero(), "a room with a live peer must not carry an idle stamp")

	_ = connB.Close()
	require.NoError(t, s.Shutdown(context.Background()))
}

// White-box guard: even if a room ends up with an idle stamp AND a peer present
// (the exact stale-stamp state the sweeper must be robust against), a direct
// sweep must not evict it — it rechecks emptiness under rm.mu.
func TestIdleSweep_ReCheckPeersUnderLock(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	s.RoomIdleTimeout = 10 * time.Millisecond
	waitUnloaded := newUnloadSignal(s)
	ts := httptest.NewServer(s)
	defer ts.Close()

	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)

	// Force the pathological state: an expired idle stamp on an OCCUPIED room.
	rm, present, _ := roomState(s, "room")
	require.True(t, present)
	rm.mu.Lock()
	require.NotEmpty(t, rm.peers, "peer A must be registered")
	rm.idleSince = time.Now().Add(-time.Hour) // long expired
	rm.mu.Unlock()

	// Direct sweep: must skip the occupied room despite the expired stamp.
	s.sweepIdleRooms(time.Now())

	_, stillPresent, _ := roomState(s, "room")
	assert.True(t, stillPresent, "sweeper must not evict a room that still has a peer")
	assert.False(t, waitUnloaded(t, "room", 50*time.Millisecond),
		"OnUnloadDocument must not fire for an occupied room")

	_ = connA.Close()
	require.NoError(t, s.Shutdown(context.Background()))
}

// With MaxResidentRooms=N, creating the (N+1)-th idle room must trigger LRU
// eviction of the least-recently-idle room (smallest idleSince) first.
func TestIdleSweep_MaxResidentRoomsLRU(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	// A long timeout so timeout-based eviction never fires — isolate LRU.
	s.RoomIdleTimeout = 10 * time.Second
	s.idleSweepInterval = 5 * time.Millisecond
	s.MaxResidentRooms = 2
	waitUnloaded := newUnloadSignal(s)
	lastPeer := newLastPeerSignal(s)
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Create three idle rooms with strictly increasing idleSince stamps.
	makeIdle := func(room string) {
		doc := crdt.New(crdt.WithClientID(1))
		conn := dialWS(t, ts, room)
		drainWS(t, conn, doc)
		_ = conn.Close()
		<-lastPeer // stamped idle
	}
	makeIdle("room1") // least-recently-idle
	time.Sleep(3 * time.Millisecond)
	makeIdle("room2")
	time.Sleep(3 * time.Millisecond)
	makeIdle("room3") // most-recently-idle; pushes idle count to 3 > Max(2)

	require.True(t, waitUnloaded(t, "room1", 2*time.Second),
		"LRU: the least-recently-idle room (room1) must be evicted first")

	_, p1, _ := roomState(s, "room1")
	_, p2, _ := roomState(s, "room2")
	_, p3, _ := roomState(s, "room3")
	assert.False(t, p1, "room1 (oldest idle) must be evicted by the LRU bound")
	assert.True(t, p2, "room2 must remain resident")
	assert.True(t, p3, "room3 must remain resident")

	require.NoError(t, s.Shutdown(context.Background()))
}

// The sweeper must stop on Shutdown: Shutdown returns and the sweeper loop
// exits (its done channel closes). RoomIdleTimeout==0 must start no sweeper.
func TestIdleSweep_StopsOnShutdown(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	s.RoomIdleTimeout = time.Minute
	s.idleSweepInterval = 5 * time.Millisecond
	ts := httptest.NewServer(s)

	// A room creation must lazily start the sweeper.
	doc := crdt.New(crdt.WithClientID(1))
	conn := dialWS(t, ts, "room")
	drainWS(t, conn, doc)
	require.True(t, s.sweeperStarted.Load(), "sweeper must start lazily when RoomIdleTimeout>0")
	_ = conn.Close()
	ts.Close()

	require.NoError(t, s.Shutdown(context.Background()))
	select {
	case <-s.sweeperDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper loop did not exit after Shutdown")
	}
}

// RoomIdleTimeout==0 (eager-evict) must start no sweeper at all.
func TestIdleSweep_ZeroTimeoutStartsNoSweeper(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	// RoomIdleTimeout left at zero.
	ts := httptest.NewServer(s)
	defer ts.Close()

	doc := crdt.New(crdt.WithClientID(1))
	conn := dialWS(t, ts, "room")
	drainWS(t, conn, doc)
	_ = conn.Close()

	assert.False(t, s.sweeperStarted.Load(), "RoomIdleTimeout=0 must start no sweeper")
	require.NoError(t, s.Shutdown(context.Background()))
}
