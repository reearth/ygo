package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
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

// dialWSHeader dials the test server like dialWS but attaches extra request
// headers to the handshake (used to mark a specific connection so a test hook
// can single it out). Registers a t.Cleanup to close the conn at test end.
func dialWSHeader(t *testing.T, ts *httptest.Server, room string, h http.Header) *gws.Conn {
	t.Helper()
	u := "ws" + ts.URL[len("http"):] + "/" + room
	conn, _, err := gws.DefaultDialer.Dial(u, h)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
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

// A last-peer-leave that overlaps a concurrent rejoin must never leave a stale
// idle stamp on a room that ends up occupied (#183). This reproduces the exact
// causal-point bug: when idleSince was cleared at room LOOKUP (getOrCreateRoom),
// the joining peer B cleared it BEFORE it had registered in rm.peers, then the
// sole departing peer A — seeing the room empty (B not yet registered) — flushed
// and stamped idleSince=now, and B finally registered onto an occupied room
// whose stale stamp nothing cleared. Task 12's sweeper could then evict a room
// holding a LIVE peer.
//
// The interleaving is forced deterministically rather than raced: the joining
// peer B is parked inside the WebSocket Upgrade (via a CheckOrigin that blocks),
// which is exactly the window between getOrCreateRoom returning and the peer
// registering in rm.peers. While B is parked we drop A and wait for its teardown
// stamp to complete, then release B so it registers onto the just-stamped room.
//
//   - pre-fix: registration did not clear idleSince → the stamp survives on the
//     occupied room → RED.
//   - fixed:   registration clears idleSince in the same rm.mu section that adds
//     the peer, after the stamp → idleSince==zero → GREEN.
func TestIdleRoom_ConcurrentLeaveRejoinNoStaleStampOnOccupiedRoom(t *testing.T) {
	a := &idleRecordAdapter{}
	s := NewServerWithPersistence(a)
	s.RoomIdleTimeout = time.Minute // idle mode: empty rooms are stamped, not evicted

	// Force the leave/rejoin interleaving. CheckOrigin runs inside Upgrade —
	// after getOrCreateRoom has returned but before the peer is registered in
	// rm.peers — so blocking there parks the joining peer B precisely in the
	// lookup→registration window. Only B carries the marker header; A and any
	// other dial pass straight through.
	bInUpgrade := make(chan struct{}) // closed once B is parked mid-Upgrade
	releaseB := make(chan struct{})   // closed to let B finish upgrading + register
	var bParkOnce sync.Once
	s.upgrader.CheckOrigin = func(r *http.Request) bool {
		if r.Header.Get("X-Test-Slow-Join") == "1" {
			bParkOnce.Do(func() { close(bInUpgrade) })
			<-releaseB
		}
		return true
	}

	lastPeer := newLastPeerSignal(s)
	ts := httptest.NewServer(s)
	defer ts.Close()

	// A joins and fully registers.
	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)

	// B starts dialing with the slow-join marker and blocks mid-Upgrade: past
	// getOrCreateRoom (which, on pre-fix code, has just cleared idleSince) but
	// not yet registered in rm.peers.
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		docB := crdt.New(crdt.WithClientID(2))
		h := http.Header{"X-Test-Slow-Join": {"1"}}
		connB := dialWSHeader(t, ts, "room", h)
		drainWS(t, connB, docB)
	}()
	<-bInUpgrade // B is now parked in the lookup→registration window.

	// Drop A while B is parked. A is the only registered peer, so
	// handleDisconnect sees the room empty, flushes durably, and STAMPS
	// idleSince. OnLastPeer fires after that stamp decision, so its receipt is a
	// barrier: idleSince is settled by the time we read it below.
	_ = connA.Close()
	<-lastPeer

	// At this instant the room is genuinely empty and MUST be stamped idle — the
	// fix must not suppress legitimate stamping.
	_, present, idleEmpty := roomState(s, "room")
	require.True(t, present, "idle room must stay resident (RoomIdleTimeout > 0)")
	require.False(t, idleEmpty.IsZero(), "a genuinely-empty room must be stamped idle")

	// Release B: it finishes upgrading and registers onto the stamped room.
	close(releaseB)
	<-bDone // B has received its handshake (sent only after registration).

	// The room now holds a LIVE, registered peer. Its idle stamp MUST be gone.
	rm, present, idleOccupied := roomState(s, "room")
	require.True(t, present)
	require.NotNil(t, rm)
	assert.True(t, idleOccupied.IsZero(),
		"a room with a live registered peer must NOT carry a stale idle stamp (got %v)", idleOccupied)

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
