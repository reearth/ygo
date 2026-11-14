package websocket

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A join past getOrCreateRoom but not yet in rm.peers holds inflight, so
// CloseRoom without force must keep the room for it, as evictIdleRoom does.
func TestCloseRoom_NoForce_KeepsRoomForInflightJoiner(t *testing.T) {
	s := NewServer()

	rm, _, err := s.getOrCreateRoom(context.Background(), "r")
	require.NoError(t, err)

	require.ErrorIs(t, s.CloseRoom("r", false), ErrRoomHasPeers)
	s.rmu.RLock()
	current := s.rooms["r"]
	s.rmu.RUnlock()
	assert.Same(t, rm, current, "the room the joiner holds must stay in the server's map")

	s.releaseInflight(rm)
	require.NoError(t, s.CloseRoom("r", false))
	assert.Equal(t, 0, roomCount(s), "CloseRoom must remove the room once no join is in progress")
}
