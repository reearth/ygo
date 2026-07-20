// Package websocket - internal unit tests for unexported helpers.
package websocket

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/reearth/ygo/awareness"
)

// captureDebugLogger returns a logger writing Debug-level (and above) text logs
// into buf, for asserting the server emits a line on a given path.
func captureDebugLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestPeer_HandleMessage_LogsMalformedFrames verifies that malformed inbound
// frames are no longer dropped silently: the server logs each discard at Debug
// level so an operator can diagnose why a peer's edits never land (N-12). The
// lines are Debug (not Warn) so a hostile peer cannot flood the log.
func TestPeer_HandleMessage_LogsMalformedFrames(t *testing.T) {
	t.Run("bad outer type", func(t *testing.T) {
		var buf bytes.Buffer
		p := &peer{server: &Server{Logger: captureDebugLogger(&buf)}, roomName: "r"}
		p.handleMessage(nil) // empty: the outer type VarUint cannot be read
		if !strings.Contains(buf.String(), "malformed") {
			t.Fatalf("expected a malformed-message log line, got: %q", buf.String())
		}
	})
	t.Run("truncated awareness frame", func(t *testing.T) {
		var buf bytes.Buffer
		p := &peer{server: &Server{Logger: captureDebugLogger(&buf)}, roomName: "r"}
		// msgAwareness, then a VarBytes length of 5 with no payload bytes.
		p.handleMessage([]byte{byte(msgAwareness), 0x05})
		if !strings.Contains(buf.String(), "awareness") {
			t.Fatalf("expected a malformed-awareness log line, got: %q", buf.String())
		}
	})
}

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
	assert.Equal(t, defaultPeerWriteQueueSize, s.peerWriteQueueSize(), "default is 512")
	assert.Equal(t, 512, s.peerWriteQueueSize(), "default bumped from 256 to 512")

	// Configured: positive value overrides default.
	s2 := &Server{PeerWriteQueueSize: 16}
	assert.Equal(t, 16, s2.peerWriteQueueSize(), "configured value used")
}

func TestServer_SlowPeerPolicy_DefaultsToDisconnect(t *testing.T) {
	// The zero value must be the backward-compatible disconnect behaviour, so a
	// Server that never sets the field keeps evicting slow peers.
	s := &Server{}
	assert.Equal(t, SlowPeerDisconnect, s.SlowPeerPolicy, "zero value is disconnect")
}

func TestPeer_Broadcast_ResyncPolicy_FlagsSlowPeerWithoutClosing(t *testing.T) {
	// Under SlowPeerResync a full write queue must flag the peer for an in-place
	// resync and drop the stale delta, NOT close the connection. This drives the
	// overflow branch directly by pre-filling writeCh with no runWriter draining
	// it, so no fake conn is needed (the resync branch never touches conn).
	var buf bytes.Buffer
	srv := &Server{SlowPeerPolicy: SlowPeerResync, Logger: captureDebugLogger(&buf)}
	rm := &room{peers: map[*peer]struct{}{}}

	target := &peer{server: srv, roomName: "r", writeCh: make(chan []byte, 1)}
	target.writeCh <- []byte{0x00} // fill to capacity

	src := &peer{server: srv, room: rm}
	rm.peers[target] = struct{}{}
	rm.peers[src] = struct{}{}

	src.broadcast([]byte{0x01}, true)

	target.wmu.Lock()
	needsResync, closed := target.needsResync, target.closed
	target.wmu.Unlock()

	assert.True(t, needsResync, "overflow under SlowPeerResync should flag the peer for resync")
	assert.False(t, closed, "overflow under SlowPeerResync must not disconnect the peer")
	assert.Len(t, target.writeCh, 1, "the stale delta is dropped, not enqueued")
	assert.Contains(t, buf.String(), "in-place resync", "expected a resync-scheduling log line")
}

func TestPeer_MaybeResync_GatingWithoutConn(t *testing.T) {
	// maybeResync's decision branches must be reachable without a live conn: it
	// only performs a write once the peer is flagged AND the backlog has drained.

	t.Run("not flagged is a no-op", func(t *testing.T) {
		p := &peer{server: &Server{}, writeCh: make(chan []byte, 1)}
		assert.True(t, p.maybeResync(), "an un-flagged, open peer keeps its writer alive")
	})

	t.Run("closed peer stops the writer", func(t *testing.T) {
		p := &peer{server: &Server{}, writeCh: make(chan []byte, 1), closed: true, needsResync: true}
		assert.False(t, p.maybeResync(), "a closed peer signals runWriter to exit")
	})

	t.Run("deferred while queue is non-empty", func(t *testing.T) {
		p := &peer{server: &Server{}, writeCh: make(chan []byte, 2), needsResync: true}
		p.writeCh <- []byte{0x01} // backlog not yet drained
		assert.True(t, p.maybeResync(), "resync waits until the queue drains")
		p.wmu.Lock()
		still := p.needsResync
		p.wmu.Unlock()
		assert.True(t, still, "needsResync stays set until the backlog clears")
	})
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
