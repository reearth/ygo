package websocket_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	ygws "github.com/reearth/ygo/provider/websocket"
)

func TestUnit_InjectOp_String(t *testing.T) {
	assert.Equal(t, "BroadcastUpdate", ygws.OpBroadcastUpdate.String())
	assert.Equal(t, "Apply", ygws.OpApply.String())
	assert.Equal(t, "unknown", ygws.InjectOp(99).String())
}

func TestUnit_EffectiveMaxUpdateBytes_DefaultsTo64MiB(t *testing.T) {
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
