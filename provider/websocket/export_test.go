package websocket

import "context"

// MemoryPersistenceRecordCount reports how many stored records a room holds.
// Test-only: the record count is the only way to tell a real compaction from
// a KeepVersions=0 no-op, which preserves content either way (#186).
func MemoryPersistenceRecordCount(m *MemoryPersistence, room string) int {
	vs, err := m.adapter.Store().ListVersions(context.Background(), room)
	if err != nil {
		return -1
	}
	return len(vs)
}

// MemoryPersistencePendingRooms reports how many rooms hold un-compacted
// bookkeeping. Test-only: proves the pending map is deleted, not zeroed.
func MemoryPersistencePendingRooms(m *MemoryPersistence) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}
