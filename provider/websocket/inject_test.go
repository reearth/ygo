package websocket_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
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
