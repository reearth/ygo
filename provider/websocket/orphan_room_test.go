package websocket

import (
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
