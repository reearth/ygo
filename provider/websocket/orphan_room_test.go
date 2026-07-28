package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roomCount reports how many rooms are resident in s.rooms.
func roomCount(s *Server) int {
	s.rmu.RLock()
	defer s.rmu.RUnlock()
	return len(s.rooms)
}

// A plain (non-WebSocket) GET makes gorilla's Upgrade fail AFTER getOrCreateRoom
// has created the room — the exact orphan condition from #192. The room must not
// linger. Covers eager-evict mode (default; RoomIdleTimeout == 0).
func TestServeHTTP_UpgradeFailure_ReapsOrphanRoom(t *testing.T) {
	s := NewServer()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("room", "orphan")
		s.ServeHTTP(w, r)
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL) // not a WS handshake → Upgrade fails
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, 0, roomCount(s), "room created for a request that never registered a peer must be reaped")
}

// In idle-residency mode the sweeper only reaps idleSince != 0 rooms; an orphan
// (idleSince == 0) would leak without the explicit cleanup. Verify direct reap.
func TestServeHTTP_UpgradeFailure_ReapsOrphanRoom_IdleMode(t *testing.T) {
	s := NewServer()
	s.RoomIdleTimeout = time.Hour // idle-residency mode; sweeper won't help an orphan
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("room", "orphan")
		s.ServeHTTP(w, r)
	}))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 0, roomCount(s))
}

// TestEvictIdleRoom_BlockedByInflightJoiner proves the #193-review fix: a
// caller that has obtained a room via getOrCreateRoom but not yet released it
// (a stand-in for a concurrent WS joiner still in the window between
// getOrCreateRoom returning and peer registration) must block evictIdleRoom,
// even though the room is empty and idleSince is the zero value — exactly the
// state a #192 orphan-reap sees. Once the caller releases, the room reaps.
func TestEvictIdleRoom_BlockedByInflightJoiner(t *testing.T) {
	s := NewServer()

	rm, created, err := s.getOrCreateRoom(context.Background(), "r")
	require.NoError(t, err)
	require.True(t, created)

	// Sanity: this call left the room with an in-flight joiner and no
	// registered peer / idle stamp — the exact ambiguous state the guard
	// must disambiguate.
	rm.mu.Lock()
	inflight := rm.inflight
	rm.mu.Unlock()
	require.Equal(t, 1, inflight, "getOrCreateRoom must leave inflight==1 until released")

	evicted := s.evictIdleRoom("r", rm, time.Time{})
	assert.False(t, evicted, "evictIdleRoom must refuse to evict while a joiner is inflight")
	assert.Equal(t, 1, roomCount(s), "room must still be resident while inflight > 0")

	s.releaseInflight(rm)

	evicted = s.evictIdleRoom("r", rm, time.Time{})
	assert.True(t, evicted, "evictIdleRoom must evict once inflight drops to 0")
	assert.Equal(t, 0, roomCount(s), "room must be gone once the guard clears")
}
