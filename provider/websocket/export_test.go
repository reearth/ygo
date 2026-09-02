package websocket

import (
	"context"

	"github.com/reearth/ygo/persistence"
)

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
	return len(m.rooms)
}

// MemoryPersistencePendingCount reports room's outstanding (un-folded) write
// count, 0 if the room has no entry. Test-only: distinguishes "no un-folded
// writes" from "the bookkeeping entry was erased while a write it did not
// fold was in flight" (PR #230 review, server.go:435) — MemoryPersistencePendingRooms
// only reports how many rooms have an entry, not what each one holds.
func MemoryPersistencePendingCount(m *MemoryPersistence, room string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.rooms[room]
	if l == nil {
		return 0
	}
	return int(l.outstanding())
}

// NewMemoryPersistenceForTest builds a MemoryPersistence around an arbitrary
// *persistence.LegacyAdapter, bypassing NewMemoryPersistence's fixed
// persistence.NewMemoryPersistence() store. Test-only: lets a test park
// inside the wrapped store's Compact via a custom persistence.VersionedPersistence,
// making the window between MemoryPersistence.Compact's pending snapshot and
// the fold completing deterministic to hit, without sleeps or timing luck.
func NewMemoryPersistenceForTest(adapter *persistence.LegacyAdapter) *MemoryPersistence {
	return &MemoryPersistence{adapter: adapter, rooms: make(map[string]*compactLedger)}
}

// MemoryPersistenceBackoffState reports room's consecutive-failure count and
// its next-attempt mark, or (0, 0) when the room has no ledger entry.
//
// Test-only, and the ONLY way to observe the reset-on-success: when a fold
// succeeds and leaves nothing outstanding, dropIfIdleLocked deletes the entry
// outright, which clears the same state incidentally. A test that asserts on
// cadence alone therefore passes with the reset deleted (verified by
// mutation) — the reset only does work when the ledger SURVIVES a successful
// fold, which needs a write landing inside the fold window.
func MemoryPersistenceBackoffState(m *MemoryPersistence, room string) (failures int, retryAt int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.rooms[room]
	if l == nil {
		return 0, 0
	}
	return l.failures, l.retryAt
}
