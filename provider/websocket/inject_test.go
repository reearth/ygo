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
