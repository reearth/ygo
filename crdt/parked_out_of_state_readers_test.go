package crdt

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The base an update log records is the state SetParkedOutOfState governs:
// on, it equals the base of a room that never received what it parks; off,
// it carries the parked struct and deletion.
func TestUnit_ParkedOutOfState_TheUpdateLogBaseCarriesNothingParked(t *testing.T) {
	logBase := func(room *Doc) []byte {
		log := room.OpenUpdateLog()
		defer log.Close()
		room.Transact(func(*Transaction) {})
		base, _ := log.Drain()
		require.NotNil(t, base, "the first commit records a base")
		return base
	}
	control, _ := outOfStateRoom(t, false, true)
	want := logBase(control)
	for name, on := range map[string]bool{"on": true, "off": false} {
		t.Run(name, func(t *testing.T) {
			room, _ := outOfStateRoom(t, on, false)
			assert.Equal(t, on, bytes.Equal(logBase(room), want), "equal to the base of a room that parked nothing")
			assert.Equal(t, 1, room.PendingStats().Items, "the queue still holds what parked")
		})
	}
}

// Clone keeps the setting, and with it the encodes.
func TestUnit_ParkedOutOfState_CloneKeepsTheSetting(t *testing.T) {
	for name, on := range map[string]bool{"on": true, "off": false} {
		t.Run(name, func(t *testing.T) {
			room, _ := outOfStateRoom(t, on, false)
			clone := room.Clone()
			assert.Equal(t, on, clone.ParkedOutOfState())
			assert.Equal(t, EncodeStateAsUpdateV1(room, nil), EncodeStateAsUpdateV1(clone, nil))
			assert.Equal(t, room.PendingStats(), clone.PendingStats())
		})
	}
}
