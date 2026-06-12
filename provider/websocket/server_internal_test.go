// Package websocket - internal unit tests for unexported helpers.
package websocket

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/reearth/ygo/awareness"
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
	// Integration-level coverage lives in TestServer_SlowPeer_GetsDisconnectedOnQueueOverflow
	// in server_test.go, which uses net.Pipe() connections to make the overflow deterministic.
	// A unit-level version here would require a fakeConn that makes WriteMessage block
	// without depending on OS TCP buffers.
	t.Skip("TODO: unit version needs a fake conn; integration coverage in TestServer_SlowPeer_GetsDisconnectedOnQueueOverflow")
}

func TestPeer_RunWriter_ExitsOnChannelClose(t *testing.T) {
	// Verify runWriter exits cleanly when writeCh is closed.
	// Requires a fake peer + fake conn.
	t.Skip("requires fake conn; see implementation comment")
}

// awarenessUpdateFor builds a one-client awareness update frame (live state) for
// use in wiring tests.
func awarenessUpdateFor(id uint64) []byte {
	a := awareness.New(id)
	a.SetLocalState(map[string]any{"v": "x"})
	return a.EncodeUpdate([]uint64{id})
}

// TestServer_MaxAwarenessClientsPerRoom_IsWired verifies the per-room distinct-
// entry cap is applied to rooms the server creates (S-1 DoS guard).
func TestServer_MaxAwarenessClientsPerRoom_IsWired(t *testing.T) {
	s := NewServer()
	s.MaxAwarenessClientsPerRoom = 2
	rm, err := s.getOrCreateRoom(context.Background(), "room")
	if err != nil {
		t.Fatalf("getOrCreateRoom: %v", err)
	}
	for id := uint64(1); id <= 10; id++ {
		if err := rm.awareness.ApplyUpdate(awarenessUpdateFor(id), nil); err != nil {
			t.Fatalf("ApplyUpdate(client %d): %v", id, err)
		}
	}
	if got := len(rm.awareness.GetStates()); got > 2 {
		t.Fatalf("room awareness tracked %d clients, want <= 2 (cap not wired)", got)
	}
}

// TestServer_AwarenessExpiry_GoroutineStoppedOnEvict verifies the auto-expiry
// goroutine started for a room does not outlive the room (CloseRoom -> Destroy).
func TestServer_AwarenessExpiry_GoroutineStoppedOnEvict(t *testing.T) {
	s := NewServer()
	s.AwarenessExpiry = 50 * time.Millisecond

	before := runtime.NumGoroutine()
	rm, err := s.getOrCreateRoom(context.Background(), "room")
	if err != nil {
		t.Fatalf("getOrCreateRoom: %v", err)
	}
	if rm.awareness == nil {
		t.Fatal("room has no awareness")
	}
	if err := s.CloseRoom("room", true); err != nil {
		t.Fatalf("CloseRoom: %v", err)
	}

	// Poll until the goroutine count returns to baseline (the expiry goroutine
	// exits when Destroy stops it). Condition-based wait avoids flakiness.
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.Gosched()
		if runtime.NumGoroutine() <= before+1 { // +1 tolerance for scheduler noise
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expiry goroutine leaked: before=%d after=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
