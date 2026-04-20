package websocket_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

func TestUnit_InjectOp_String(t *testing.T) {
	assert.Equal(t, "BroadcastUpdate", ygws.OpBroadcastUpdate.String())
	assert.Equal(t, "Apply", ygws.OpApply.String())
	assert.Equal(t, "unknown", ygws.InjectOp(99).String())
}

func TestUnit_Server_MaxUpdateBytesField_Exists(t *testing.T) {
	srv := ygws.NewServer()
	// MaxUpdateBytes defaults to 0 → effective value should be 64 MiB.
	// We verify via behavior in later tasks; here we just assert field
	// presence by assigning.
	srv.MaxUpdateBytes = 1024
	assert.Equal(t, 1024, srv.MaxUpdateBytes)
}

func TestUnit_Server_MaxRoomsField_Exists(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxRooms = 5
	assert.Equal(t, 5, srv.MaxRooms)
}

func TestUnit_Server_OnInjectField_Exists(t *testing.T) {
	srv := ygws.NewServer()
	srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error { return nil }
	assert.NotNil(t, srv.OnInject)
}

func TestUnit_PeerUpgrade_MaxRoomsExceeded_Returns503(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxRooms = 1
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(httpSrv.Close)

	// First peer in room-A succeeds.
	connA := dial(t, httpSrv, "room-A")
	drainHandshake(t, connA, crdt.New())

	// Peer attempting to open a second room fails with 503 on upgrade.
	resp, err := http.Get(httpSrv.URL + "/room-B")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestUnit_BroadcastUpdate_FansOutToConnectedPeers(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)

	// Open two peer connections and drain their handshakes.
	conn1 := dial(t, httpSrv, "room")
	peerDoc1 := crdt.New()
	drainHandshake(t, conn1, peerDoc1)

	conn2 := dial(t, httpSrv, "room")
	peerDoc2 := crdt.New()
	drainHandshake(t, conn2, peerDoc2)

	// Build an update externally: new doc, set a map key, encode.
	external := crdt.New()
	extMap := external.GetMap("m")
	external.Transact(func(txn *crdt.Transaction) { extMap.Set(txn, "k", "v") })
	update := crdt.EncodeStateAsUpdateV1(external, nil)

	// Apply to server's doc AND broadcast (the documented pattern).
	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	require.NoError(t, crdt.ApplyUpdateV1(serverDoc, update, nil))
	require.NoError(t, srv.BroadcastUpdate(context.Background(), "room", update))

	// Both peers receive a sync update frame.
	for i, conn := range []*gws.Conn{conn1, conn2} {
		outerType, payload := readOne(t, conn, 2*time.Second)
		assert.Equal(t, uint64(0), outerType, "peer %d should receive msgSync", i+1)
		peerDoc := []*crdt.Doc{peerDoc1, peerDoc2}[i]
		_, _ = ygsync.ApplySyncMessage(peerDoc, payload, nil)
		got, _ := peerDoc.GetMap("m").Get("k")
		assert.Equal(t, "v", got)
	}
}

func TestUnit_BroadcastUpdate_MissingRoom_ErrRoomNotFound(t *testing.T) {
	srv := ygws.NewServer()
	// Build a valid update to pass the parse check; we expect to fail at room lookup.
	d := crdt.New()
	dMap := d.GetMap("m")
	d.Transact(func(txn *crdt.Transaction) { dMap.Set(txn, "k", "v") })
	update := crdt.EncodeStateAsUpdateV1(d, nil)
	err := srv.BroadcastUpdate(context.Background(), "ghost", update)
	assert.ErrorIs(t, err, ygws.ErrRoomNotFound)
}

func TestUnit_BroadcastUpdate_InvalidRoomName(t *testing.T) {
	srv := ygws.NewServer()
	for _, name := range []string{"", "..", ".", "\x01bad", strings.Repeat("x", 256)} {
		err := srv.BroadcastUpdate(context.Background(), name, []byte{0x00, 0x00})
		assert.ErrorIs(t, err, ygws.ErrInvalidRoomName, "name=%q", name)
	}
}

func TestUnit_BroadcastUpdate_ContextAlreadyCancelled(t *testing.T) {
	srv := ygws.NewServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := srv.BroadcastUpdate(ctx, "room", []byte{0x00, 0x00})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestUnit_BroadcastUpdate_ShutdownServer(t *testing.T) {
	srv := ygws.NewServer()
	require.NoError(t, srv.Shutdown(context.Background()))
	err := srv.BroadcastUpdate(context.Background(), "room", []byte{0x00, 0x00})
	assert.ErrorIs(t, err, ygws.ErrServerShutdown)
}

func TestUnit_BroadcastUpdate_UpdateTooLarge(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxUpdateBytes = 16
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	err := srv.BroadcastUpdate(context.Background(), "room", make([]byte, 32))
	assert.ErrorIs(t, err, ygws.ErrUpdateTooLarge)
}

func TestUnit_BroadcastUpdate_InvalidUpdateBytes(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	err := srv.BroadcastUpdate(context.Background(), "room", []byte{0xff, 0xff, 0xff, 0xff})
	assert.ErrorIs(t, err, ygws.ErrInvalidUpdate)
}

func TestUnit_BroadcastUpdate_DoesNotMutateServerDoc(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	external := crdt.New()
	extMap := external.GetMap("m")
	external.Transact(func(txn *crdt.Transaction) { extMap.Set(txn, "k", "v") })
	update := crdt.EncodeStateAsUpdateV1(external, nil)

	require.NoError(t, srv.BroadcastUpdate(context.Background(), "room", update))

	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	got, ok := serverDoc.GetMap("m").Get("k")
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestUnit_Apply_MutatesBroadcastsAndPersists(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	peerDoc := crdt.New()
	drainHandshake(t, conn, peerDoc)

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m") // MUST be outside transact — see spec
		transact(func(txn *crdt.Transaction) {
			m.Set(txn, "k", "v")
		})
	})
	require.NoError(t, err)

	// Peer receives the update.
	outerType, payload := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType)
	_, _ = ygsync.ApplySyncMessage(peerDoc, payload, nil)
	got, ok := peerDoc.GetMap("m").Get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)

	// Server doc also reflects the change.
	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	got, ok = serverDoc.GetMap("m").Get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)
}

func TestUnit_Apply_EmptyFn_ErrNoChanges(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		// never call transact
	})
	assert.ErrorIs(t, err, ygws.ErrNoChanges)
}

func TestUnit_Apply_InvalidRoomName(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.Apply(context.Background(), "..", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {})
	assert.ErrorIs(t, err, ygws.ErrInvalidRoomName)
}

func TestUnit_Apply_ContextAlreadyCancelled(t *testing.T) {
	srv := ygws.NewServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fnCalled := false
	err := srv.Apply(ctx, "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fnCalled = true
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, fnCalled, "fn must not be called when ctx is already cancelled")
}

func TestUnit_Apply_MultipleTransacts_MergedAndBroadcastOnce(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	peerDoc := crdt.New()
	drainHandshake(t, conn, peerDoc)

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k1", "v1") })
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k2", "v2") })
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k3", "v3") })
	})
	require.NoError(t, err)

	outerType, payload := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType)
	_, _ = ygsync.ApplySyncMessage(peerDoc, payload, nil)

	for key, want := range map[string]string{"k1": "v1", "k2": "v2", "k3": "v3"} {
		got, ok := peerDoc.GetMap("m").Get(key)
		require.True(t, ok, "peer missing key %s", key)
		assert.Equal(t, want, got)
	}

	// No second message pending.
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	assert.Error(t, err, "expected no further messages")
}

func TestUnit_Apply_AutoCreatesRoom(t *testing.T) {
	srv := ygws.NewServer()
	assert.Nil(t, srv.GetDoc("new-room"))

	err := srv.Apply(context.Background(), "new-room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
	})
	require.NoError(t, err)

	doc := srv.GetDoc("new-room")
	require.NotNil(t, doc)
	got, ok := doc.GetMap("m").Get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)
}

func TestUnit_Apply_MaxRoomsExceeded(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxRooms = 2

	for i, name := range []string{"a", "b"} {
		err := srv.Apply(context.Background(), name, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			m := doc.GetMap("m")
			transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
		})
		require.NoError(t, err, "room %d %q should succeed", i, name)
	}

	err := srv.Apply(context.Background(), "c", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {})
	assert.ErrorIs(t, err, ygws.ErrTooManyRooms)
	assert.Nil(t, srv.GetDoc("c"), "failed Apply must not leave a partial room")
}

func TestUnit_Apply_UpdateTooLarge(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxUpdateBytes = 32

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := doc.GetText("t")
		transact(func(txn *crdt.Transaction) {
			txt.Insert(txn, 0, strings.Repeat("x", 1000), nil)
		})
	})
	assert.ErrorIs(t, err, ygws.ErrUpdateTooLarge)

	// Doc HAS been mutated (post-hoc size check).
	doc := srv.GetDoc("room")
	require.NotNil(t, doc)
	assert.Equal(t, 1000, doc.GetText("t").Len())
}

func TestUnit_Apply_AfterShutdown(t *testing.T) {
	srv := ygws.NewServer()
	require.NoError(t, srv.Shutdown(context.Background()))
	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {})
	assert.ErrorIs(t, err, ygws.ErrServerShutdown)
}

// NOTE: a test that panics INSIDE transact is intentionally omitted.
// The pre-existing crdt.Doc.Transact panic-unlock bug (tracked as a
// separate follow-up issue) means such a panic leaves d.mu held, and
// Apply's defer-unsub — which needs d.mu — deadlocks. Apply's doc
// comment instructs callers: "fn MUST NOT panic." The BeforeTransact
// test below is the maximal safety guarantee we can verify today.

func TestUnit_Apply_FnPanic_BeforeTransact_NoLeak(t *testing.T) {
	srv := ygws.NewServer()

	func() {
		defer func() { _ = recover() }()
		_ = srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			panic("before transact")
		})
	}()

	// The panic was BEFORE transact, so the doc's write lock was
	// never acquired — the room's doc is still usable.
	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
	})
	assert.NoError(t, err)
}

func TestUnit_Apply_FnBypassesTransactHelper_ErrNoChangesButDocMutated(t *testing.T) {
	srv := ygws.NewServer()

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		// BYPASS: caller goes directly to doc.Transact instead of the
		// supplied transact helper.
		doc.Transact(func(txn *crdt.Transaction) {
			m.Set(txn, "k", "v")
		})
	})
	assert.ErrorIs(t, err, ygws.ErrNoChanges, "bypass should report ErrNoChanges")

	// Doc IS mutated — well-defined but surprising behavior; documented.
	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	got, ok := serverDoc.GetMap("m").Get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)
}

func TestUnit_Apply_TriggersPersistenceViaOnUpdate(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	srv := ygws.NewServerWithPersistence(p)

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
	})
	require.NoError(t, err)

	// Shutdown forces the persistence goroutine to drain its queue
	// before returning, so LoadDoc is guaranteed to see the update.
	require.NoError(t, srv.Shutdown(context.Background()))

	stored, err := p.LoadDoc("room")
	require.NoError(t, err)
	require.NotNil(t, stored)

	reloaded := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(reloaded, stored, nil))
	got, ok := reloaded.GetMap("m").Get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)
}
