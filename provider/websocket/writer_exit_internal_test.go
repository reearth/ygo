package websocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upgradedConn returns the server side of a real WebSocket connection.
func upgradedConn(t *testing.T) *gws.Conn {
	t.Helper()
	conns := make(chan *gws.Conn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&gws.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			conns <- c
		}
	}))
	t.Cleanup(ts.Close)
	client, _, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	c := <-conns
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestUnit_Peer_WriteToConn_FailedWriteDropsInput(t *testing.T) {
	// A write that fails closes the connection, and the read loop must drop
	// the frames still in its read buffer: no writer serves the peer.
	conn := upgradedConn(t)
	require.NoError(t, conn.UnderlyingConn().Close()) // every write now fails
	p := &peer{server: &Server{}, roomName: "r", conn: conn}

	assert.False(t, p.writeToConn([]byte{0x00}), "the writer stops on a failed write")
	assert.True(t, p.inputClosed.Load(), "the read loop must drop what the peer already sent")
}
