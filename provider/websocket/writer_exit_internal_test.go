package websocket

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_Peer_WriteToConn_FailedWriteDropsInput(t *testing.T) {
	// A write that fails closes the connection, and the read loop must drop
	// the frames still in its read buffer: no writer serves the peer.
	conn := serverConn(t)
	require.NoError(t, conn.UnderlyingConn().Close()) // every write now fails
	p := &peer{server: &Server{}, roomName: "r", conn: conn}

	assert.False(t, p.writeToConn([]byte{0x00}), "the writer stops on a failed write")
	assert.True(t, p.inputClosed.Load(), "the read loop must drop what the peer already sent")
}
