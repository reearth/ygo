package websocket_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// Tests for #60 — PersistenceAdapter lifecycle hooks
// (OnLoadDocument / OnUnloadDocument / OnFirstPeer / OnLastPeer) on
// provider/websocket.Server.

// OnLoadDocument fires once per room when the doc is first loaded into
// memory, with the room name and the freshly-bootstrapped *crdt.Doc.
func TestInteg_Lifecycle_OnLoadDocument_FiresOnce(t *testing.T) {
	srv := ygws.NewServer()

	var (
		mu     sync.Mutex
		calls  []string
		gotDoc *crdt.Doc
	)
	srv.OnLoadDocument = func(_ context.Context, room string, doc *crdt.Doc) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, room)
		gotDoc = doc
		return nil
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// First peer triggers room creation → OnLoadDocument fires.
	conn1 := dial(t, ts, "loadroom")
	drainHandshake(t, conn1, crdt.New())

	// Second peer in the same room must NOT fire OnLoadDocument again.
	conn2 := dial(t, ts, "loadroom")
	drainHandshake(t, conn2, crdt.New())

	// Allow brief settling time for hook dispatch.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"loadroom"}, calls,
		"OnLoadDocument must fire exactly once for the first peer to a room")
	assert.NotNil(t, gotDoc,
		"the doc passed to OnLoadDocument must not be nil")
}

// OnLoadDocument returning an error must fail room creation: the peer's
// WebSocket handshake should fail (HTTP 500 on upgrade).
func TestInteg_Lifecycle_OnLoadDocument_ErrorFailsRoomCreation(t *testing.T) {
	srv := ygws.NewServer()
	srv.OnLoadDocument = func(_ context.Context, _ string, _ *crdt.Doc) error {
		return errors.New("simulated load failure")
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Dial must fail because the upgrade returns a non-101 status.
	_, _, err := gws.DefaultDialer.Dial(wsURL(ts, "failroom"), nil)
	require.Error(t, err,
		"OnLoadDocument error must propagate as a failed WebSocket upgrade")
}

// OnUnloadDocument fires when the last peer disconnects (room is evicted
// from the server map). With a single peer that disconnects, both
// OnLastPeer and OnUnloadDocument must fire — and only OnLastPeer is
// expected to fire BEFORE OnUnloadDocument.
func TestInteg_Lifecycle_OnUnloadDocument_FiresOnLastDisconnect(t *testing.T) {
	srv := ygws.NewServer()

	var (
		mu    sync.Mutex
		order []string
	)
	srv.OnLastPeer = func(_ context.Context, room string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "last:"+room)
	}
	srv.OnUnloadDocument = func(_ context.Context, room string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "unload:"+room)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "unloadroom")
	drainHandshake(t, conn, crdt.New())
	_ = conn.Close()

	// Disconnect cleanup is async. Poll until both hooks have fired.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2
	}, 2*time.Second, 10*time.Millisecond,
		"both OnLastPeer and OnUnloadDocument must fire on last disconnect")

	mu.Lock()
	assert.Equal(t, []string{"last:unloadroom", "unload:unloadroom"}, order,
		"OnLastPeer must fire before OnUnloadDocument")
	mu.Unlock()
}

// OnFirstPeer fires only on the 0→1 transition, not on subsequent peer
// joins. OnLastPeer fires only on the 1→0 transition.
func TestInteg_Lifecycle_OnFirstPeer_OnlyOnZeroToOne(t *testing.T) {
	srv := ygws.NewServer()

	var firstCalls, lastCalls atomic.Int32
	srv.OnFirstPeer = func(_ context.Context, room string) {
		if room == "transitroom" {
			firstCalls.Add(1)
		}
	}
	srv.OnLastPeer = func(_ context.Context, room string) {
		if room == "transitroom" {
			lastCalls.Add(1)
		}
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Open three peers; only the first should trigger OnFirstPeer.
	conn1 := dial(t, ts, "transitroom")
	drainHandshake(t, conn1, crdt.New())
	conn2 := dial(t, ts, "transitroom")
	drainHandshake(t, conn2, crdt.New())
	conn3 := dial(t, ts, "transitroom")
	drainHandshake(t, conn3, crdt.New())

	time.Sleep(100 * time.Millisecond) // settle
	assert.EqualValues(t, 1, firstCalls.Load(),
		"OnFirstPeer must fire exactly once for a 0→1 transition")
	assert.EqualValues(t, 0, lastCalls.Load(),
		"OnLastPeer must NOT fire while peers are still connected")

	// Close two of the three; OnLastPeer must NOT fire yet.
	_ = conn1.Close()
	_ = conn2.Close()
	time.Sleep(200 * time.Millisecond)
	assert.EqualValues(t, 0, lastCalls.Load(),
		"OnLastPeer must NOT fire while at least one peer remains")

	// Close the last one; OnLastPeer must fire exactly once.
	_ = conn3.Close()
	require.Eventually(t, func() bool {
		return lastCalls.Load() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"OnLastPeer must fire exactly once for the 1→0 transition")
}

// Regression for #93 self-review B1 — CloseRoom must fire
// OnUnloadDocument when it is the path that evicts the room.
func TestInteg_Lifecycle_CloseRoom_FiresOnUnloadDocument(t *testing.T) {
	srv := ygws.NewServer()
	var (
		mu    sync.Mutex
		fired []string
	)
	srv.OnUnloadDocument = func(_ context.Context, room string) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, room)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Bootstrap the room via Apply (so there's a room with no peers).
	require.NoError(t, srv.Apply(context.Background(), "closeroom", func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := d.GetText("t")
		transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
	}))

	require.NoError(t, srv.CloseRoom("closeroom", false))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(fired) == 1
	}, 2*time.Second, 10*time.Millisecond,
		"CloseRoom must fire OnUnloadDocument exactly once")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"closeroom"}, fired)
}

// Regression for #93 self-review B1 — CloseRoom racing with the
// last-peer disconnect must fire OnUnloadDocument exactly once, not
// twice. Pre-fix, both paths fired the hook because handleDisconnect
// didn't check whether it actually evicted rm from s.rooms.
func TestInteg_Lifecycle_CloseRoom_VsDisconnect_FiresUnloadOnce(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxPeersPerRoom = 1
	var fireCount atomic.Int32
	srv.OnUnloadDocument = func(_ context.Context, _ string) {
		fireCount.Add(1)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Run the race ~50 times to exercise both interleavings.
	const trials = 50
	for i := 0; i < trials; i++ {
		fireCount.Store(0)
		roomName := "raceroom" + strconv.Itoa(i)

		conn := dial(t, ts, roomName)
		drainHandshake(t, conn, crdt.New())

		// Concurrently fire CloseRoom while the peer disconnects.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = conn.Close() }()
		go func() { defer wg.Done(); _ = srv.CloseRoom(roomName, true) }()
		wg.Wait()

		// Both paths drained; assert exactly one OnUnloadDocument fired.
		require.Eventually(t, func() bool {
			return fireCount.Load() >= 1
		}, 2*time.Second, 5*time.Millisecond,
			"trial %d: at least one path must fire OnUnloadDocument", i)
		// Small settle in case the loser path is mid-execution.
		time.Sleep(20 * time.Millisecond)
		got := fireCount.Load()
		require.EqualValues(t, 1, got,
			"trial %d: OnUnloadDocument fired %d times for %s; want exactly 1",
			i, got, roomName)
	}
}

// Regression for #93 self-review B2 — a panicking lifecycle hook must
// not crash the server or break the peer-disconnect cleanup path.
// Verifies via a follow-up Ping that the server is still responsive.
func TestInteg_Lifecycle_PanickingHook_DoesNotCrashServer(t *testing.T) {
	srv := ygws.NewServer()
	srv.OnFirstPeer = func(_ context.Context, _ string) {
		panic("hostile hook")
	}
	srv.OnLastPeer = func(_ context.Context, _ string) {
		panic("hostile hook")
	}
	srv.OnUnloadDocument = func(_ context.Context, _ string) {
		panic("hostile hook")
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Connect (triggers OnFirstPeer panic), then disconnect (triggers
	// OnLastPeer + OnUnloadDocument panics). The server must absorb
	// all three panics without bringing down its goroutines.
	conn := dial(t, ts, "panicroom")
	drainHandshake(t, conn, crdt.New())
	_ = conn.Close()

	// New connection on a different room — proves the server is alive.
	conn2 := dial(t, ts, "alive-check-room")
	drainHandshake(t, conn2, crdt.New())
	defer func() { _ = conn2.Close() }()
}

// All four hooks are optional — a server with none of them set must
// behave identically to the pre-#60 codebase.
func TestInteg_Lifecycle_NilHooks_NoPanic(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "nilhookroom")
	drainHandshake(t, conn, crdt.New())
	_ = conn.Close()
	// Survive the disconnect cleanup without panicking.
	time.Sleep(100 * time.Millisecond)
}
