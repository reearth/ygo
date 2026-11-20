//go:build go1.24

package persistence

import (
	"context"
	"runtime"
	"testing"
	"weak"
)

// The test in this file needs the weak package, which Go 1.24 added; the
// module still builds with Go 1.23, which leaves it out.

// saveHeld saves a large snapshot of room on m and returns its ID and a weak
// pointer to the bytes m keeps for it; no frame of the test holds them.
func saveHeld(t *testing.T, m *MemoryPersistence, room string) (int64, weak.Pointer[byte]) {
	t.Helper()
	id, err := m.SaveSnapshot(context.Background(), room, "big", make([]byte, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.rooms[room].snapVersions
	return id, weak.Make(&versions[len(versions)-1].state[0])
}

// DeleteSnapshot clears the slot it vacates in the room's snapshot list, so
// the deleted snapshot's bytes are reachable from nothing.
func TestUnit_MemoryPersistence_DeleteSnapshotClearsTheVacatedSlot(t *testing.T) {
	m := NewMemoryPersistence()
	if _, err := m.SaveSnapshot(context.Background(), "room", "keep", []byte{1}); err != nil {
		t.Fatal(err)
	}
	id, freed := saveHeld(t, m, "room")
	if err := m.DeleteSnapshot(context.Background(), "room", id); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if freed.Value() != nil {
		t.Error("the deleted snapshot's bytes are still reachable from the room's snapshot list")
	}
	runtime.KeepAlive(m)
}
