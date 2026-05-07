// Package websocket - internal unit tests for unexported helpers.
package websocket

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestServer_MaxMessageBytes_ConfigurableDefault(t *testing.T) {
	// Default: zero in config means use the maxWSMessageBytes constant.
	s := &Server{}
	assert.Equal(t, int64(64<<20), s.maxMessageBytes(), "default is 64 MiB")

	// Configured: positive value overrides default.
	s2 := &Server{MaxMessageBytes: 1 << 20}
	assert.Equal(t, int64(1<<20), s2.maxMessageBytes(), "configured value used")
}

func TestServer_Logger_FallsBackToDefault(t *testing.T) {
	s := &Server{}
	assert.NotNil(t, s.log(), "log() returns non-nil even with no Logger configured")

	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	s2 := &Server{Logger: custom}
	assert.Same(t, custom, s2.log(), "configured logger is returned")
}

func TestServer_PeerWriteQueueSize_ConfigurableDefault(t *testing.T) {
	// Default: zero in config means use the defaultPeerWriteQueueSize constant.
	s := &Server{}
	assert.Equal(t, defaultPeerWriteQueueSize, s.peerWriteQueueSize(), "default is 256")

	// Configured: positive value overrides default.
	s2 := &Server{PeerWriteQueueSize: 16}
	assert.Equal(t, 16, s2.peerWriteQueueSize(), "configured value used")
}

func TestPeer_Broadcast_DisconnectsSlowPeer(t *testing.T) {
	// Set up a server with a small queue size to exercise overflow quickly.
	// This test requires a fakeConn or real httptest server harness.
	// See TestServer_SlowPeer_GetsDisconnected in server_test.go for the
	// integration-level version.
	t.Skip("requires fakeConn or real httptest server harness; see implementation comment")
}

func TestPeer_RunWriter_ExitsOnChannelClose(t *testing.T) {
	// Verify runWriter exits cleanly when writeCh is closed.
	// Requires a fake peer + fake conn.
	t.Skip("requires fake conn; see implementation comment")
}
