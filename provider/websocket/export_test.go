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

// StrandedWritesInFlight reports how many committing goroutines are registered
// in the stranded-write path — the #229 counter Shutdown joins on. Test-only,
// and the only way to SEQUENCE a shutdown-join test deterministically: a test
// must know the committer has registered before it lets the worker exit,
// otherwise it is asserting against the acknowledged residual (a commit that
// starts after Shutdown has read the counter) rather than against the join.
func StrandedWritesInFlight(s *Server) int64 { return s.strandedInFlight.Load() }

// MemoryPersistencePendingRooms reports how many rooms hold un-compacted
// bookkeeping. Test-only: proves the pending map is deleted, not zeroed.
func MemoryPersistencePendingRooms(m *MemoryPersistence) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}
