package websocket_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	ygws "github.com/reearth/ygo/provider/websocket"
)

func TestUnit_InjectOp_String(t *testing.T) {
	assert.Equal(t, "BroadcastUpdate", ygws.OpBroadcastUpdate.String())
	assert.Equal(t, "Apply", ygws.OpApply.String())
	assert.Equal(t, "unknown", ygws.InjectOp(99).String())
}
