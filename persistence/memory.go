package persistence

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/reearth/ygo/crdt"
)

// record is one stored update.
type record struct {
	version   Version
	update    []byte
	updatedAt time.Time
}

// memRoom is the per-room state in MemoryPersistence.
type memRoom struct {
	records   []record // sorted ascending by version
	nextVer   Version  // next version to assign (starts at 1)
	snapshots map[string]snapshot
	// checkpoint is a hard ceiling on the visible version range, set by
	// PruneAfter before it deletes. Updates with version > checkpoint are never
	// returned even if they linger (crash between checkpoint-write and delete).
	//
	// checkpointSet, not the value of checkpoint, gates whether the ceiling is
	// active. We MUST NOT overload checkpoint==0 to mean "no ceiling": a
	// PruneAfter(target=0) sets a legitimate ceiling of 0 (roll back to the
	// empty/rolled-back head), and treating that as "no ceiling" would resurrect
	// lingering records 1..N after a mid-prune crash. Mirrors FilePersistence,
	// where checkpoint presence is encoded by the existence of the checkpoint
	// file, not by its value.
	checkpoint    Version
	checkpointSet bool
	// rolledBack is the V1 head persisted at checkpoint time, used to bootstrap
	// the room after a prune so Load reflects target state even if a crash left
	// stale records behind.
	rolledBack []byte
	// snapVersions holds SnapshotStore entries ascending by id. nextSnapID is
	// monotonic and never rewound, so an id is not reused after a delete.
	snapVersions []labelledSnapshot
	nextSnapID   int64
}

type snapshot struct {
	state   []byte
	version Version
}

// labelledSnapshot is one SnapshotStore entry: ID-keyed, with a non-unique
// label, independent of the update log.
type labelledSnapshot struct {
	id        int64
	label     string
	createdAt time.Time
	state     []byte
}

// MemoryPersistence is an in-memory VersionedPersistence. It is the reference
// implementation and the simplest target for the conformance suite. Safe for
// concurrent use.
type MemoryPersistence struct {
	mu    sync.Mutex
	rooms map[string]*memRoom

	// crashAfterCheckpoint, when set, is invoked by PruneAfter immediately after
	// the checkpoint + rolled-back head are written but BEFORE the deletes run.
	// If it returns true, PruneAfter returns early (simulating a crash), leaving
	// stale future records on disk. Test-only; nil in production.
	crashAfterCheckpoint func() bool
}

// NewMemoryPersistence returns an empty in-memory store.
func NewMemoryPersistence() *MemoryPersistence {
	return &MemoryPersistence{rooms: make(map[string]*memRoom)}
}

// SetCrashAfterCheckpoint satisfies CrashInjector for the conformance suite.
func (m *MemoryPersistence) SetCrashAfterCheckpoint(fn func() bool) {
	m.mu.Lock()
	m.crashAfterCheckpoint = fn
	m.mu.Unlock()
}

// visibleRecords returns the records whose version is within the room's
// checkpoint ceiling, ascending. Must be called with mu held.
func (r *memRoom) visibleRecords() []record {
	if !r.checkpointSet {
		return r.records
	}
	out := make([]record, 0, len(r.records))
	for _, rec := range r.records {
		if rec.version <= r.checkpoint {
			out = append(out, rec)
		}
	}
	return out
}

func (m *MemoryPersistence) getRoom(room string) *memRoom {
	r, ok := m.rooms[room]
	if !ok {
		r = &memRoom{nextVer: 1, snapshots: make(map[string]snapshot), nextSnapID: 1}
		m.rooms[room] = r
	}
	return r
}

// Load returns the materialized head state.
func (m *MemoryPersistence) Load(ctx context.Context, room string) (LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return LoadResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rooms[room]
	if !ok {
		return LoadResult{}, nil
	}
	return materializeLocked(r, r.checkpointOrMaxVisible())
}

// checkpointOrMaxVisible returns the ceiling version to materialize to: the
// checkpoint if set, else the highest visible record version (or 0).
func (r *memRoom) checkpointOrMaxVisible() Version {
	if r.checkpointSet {
		return r.checkpoint
	}
	recs := r.records
	if len(recs) == 0 {
		return 0
	}
	return recs[len(recs)-1].version
}

// materializeLocked folds visible records up to v into a head LoadResult.
// Must be called with mu held.
func materializeLocked(r *memRoom, v Version) (LoadResult, error) {
	recs := r.visibleRecords()
	blobs := make([][]byte, 0, len(recs)+1)
	if len(r.rolledBack) > 0 {
		blobs = append(blobs, r.rolledBack)
	}
	var head Version
	for _, rec := range recs {
		if rec.version > v {
			break
		}
		blobs = append(blobs, rec.update)
		head = rec.version
	}
	if r.checkpointSet && r.checkpoint <= v {
		head = r.checkpoint
	}
	if len(blobs) == 0 {
		return LoadResult{Version: head}, nil
	}
	merged, err := crdt.MergeUpdatesV1(blobs...)
	if err != nil {
		return LoadResult{}, err
	}
	return LoadResult{Update: merged, Version: head}, nil
}

// AppendUpdate appends one V1 update and returns its version.
func (m *MemoryPersistence) AppendUpdate(ctx context.Context, room string, update []byte) (Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Validate the update is well-formed V1 before committing a version.
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.getRoom(room)

	// Crash recovery: a PruneAfter that crashed after writing its checkpoint but
	// before its deletes leaves checkpointSet=true with stale records>checkpoint
	// still present. Finish the interrupted prune now — drop the leaked records
	// and clear the ceiling — before assigning this update's version. Otherwise
	// the new version (>checkpoint) would itself be clamped away (freeze) and the
	// leaked records would linger. Dense reuse: resume at checkpoint+1.
	if r.checkpointSet {
		kept := r.records[:0]
		for _, rec := range r.records {
			if rec.version <= r.checkpoint {
				kept = append(kept, rec)
			}
		}
		r.records = kept
		r.nextVer = r.checkpoint + 1
		r.checkpointSet = false
		r.checkpoint = 0
		r.rolledBack = nil
	}

	v := r.nextVer
	r.nextVer++
	cp := append([]byte(nil), update...)
	r.records = append(r.records, record{version: v, update: cp, updatedAt: time.Now()})
	return v, nil
}

// ListVersions returns metadata newest-first.
func (m *MemoryPersistence) ListVersions(ctx context.Context, room string) ([]VersionMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return nil, nil
	}
	recs := r.visibleRecords()
	out := make([]VersionMeta, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- {
		out = append(out, VersionMeta{Version: recs[i].version, UpdatedAt: recs[i].updatedAt})
	}
	return out, nil
}

// GetUpdate returns the single update at v.
func (m *MemoryPersistence) GetUpdate(ctx context.Context, room string, v Version) ([]byte, VersionMeta, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, VersionMeta{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return nil, VersionMeta{}, false, nil
	}
	for _, rec := range r.visibleRecords() {
		if rec.version == v {
			cp := append([]byte(nil), rec.update...)
			return cp, VersionMeta{Version: rec.version, UpdatedAt: rec.updatedAt}, true, nil
		}
	}
	return nil, VersionMeta{}, false, nil
}

// MaterializeAt rebuilds the V1 head at v.
func (m *MemoryPersistence) MaterializeAt(ctx context.Context, room string, v Version) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v == 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return nil, ErrRoomNotFound
	}
	res, err := materializeLocked(r, v)
	if err != nil {
		return nil, err
	}
	return res.Update, nil
}

// CaptureSnapshot stores a named V1 snapshot.
func (m *MemoryPersistence) CaptureSnapshot(ctx context.Context, room, name string, state []byte) (Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.getRoom(room)
	v := r.checkpointOrMaxVisible()
	r.snapshots[name] = snapshot{state: append([]byte(nil), state...), version: v}
	return v, nil
}

// RestoreSnapshot returns a named snapshot.
func (m *MemoryPersistence) RestoreSnapshot(ctx context.Context, room, name string) ([]byte, Version, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return nil, 0, false, nil
	}
	snap, ok := r.snapshots[name]
	if !ok {
		return nil, 0, false, nil
	}
	return append([]byte(nil), snap.state...), snap.version, true, nil
}

// PruneAfter implements snapshot-before-delete.
//
// Steps 1 (checkpoint write) and 2 (delete) run in a SINGLE critical section so
// a concurrent Delete(room) cannot slip in between them — without the single
// lock, step 2 would mutate a *memRoom already detached from m.rooms and
// silently lose the prune. The crash-injector hook is invoked while the lock is
// still held: the conformance suite only needs the crash to be observable AFTER
// the checkpoint is set and BEFORE the deletes, which is exactly this point. On
// a simulated crash we return with the checkpoint set but the records still
// present — readers clamp to the checkpoint (checkpointSet), so the lingering
// future records stay invisible even across a "reopen".
func (m *MemoryPersistence) PruneAfter(ctx context.Context, room string, target Version, rolledBack []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return ErrRoomNotFound
	}

	// Step 1: persist checkpoint + rolled-back head. From this instant the
	// checkpoint is the hard ceiling; any records > target are already
	// invisible to readers, even before the delete in step 2 runs. Gate on
	// checkpointSet (not the value) so target==0 is a real ceiling.
	r.checkpoint = target
	r.checkpointSet = true
	r.rolledBack = append([]byte(nil), rolledBack...)

	// Simulated crash point for the conformance crash-safety test: return before
	// the deletes, leaving stale future records behind. The checkpoint must
	// still suppress them on reopen.
	if m.crashAfterCheckpoint != nil && m.crashAfterCheckpoint() {
		return nil
	}

	// Step 2: delete records newer than target.
	kept := r.records[:0]
	for _, rec := range r.records {
		if rec.version <= target {
			kept = append(kept, rec)
		}
	}
	r.records = kept

	// Prune committed: the surviving records (<=target) now fully reconstruct the
	// head, so the checkpoint ceiling has done its job and MUST be cleared —
	// otherwise visibleRecords() would clamp to target forever and freeze the
	// room, hiding any later AppendUpdate. The rolled-back head is likewise no
	// longer needed (records<=target rebuild it). Dense reuse: the next append
	// continues at target+1.
	r.checkpointSet = false
	r.checkpoint = 0
	r.rolledBack = nil
	r.nextVer = target + 1
	return nil
}

// Compact folds the oldest updates into the oldest retained one, keeping at
// most keep newest records.
func (m *MemoryPersistence) Compact(ctx context.Context, room string, keep int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return 0, nil
	}
	recs := r.visibleRecords()
	if len(recs) <= keep {
		return 0, nil
	}
	trimEnd := len(recs) - keep // [0:trimEnd) folded into recs[trimEnd]
	blobs := make([][]byte, 0, trimEnd+1)
	for i := 0; i <= trimEnd; i++ {
		blobs = append(blobs, recs[i].update)
	}
	merged, err := crdt.MergeUpdatesV1(blobs...)
	if err != nil {
		return 0, err
	}
	// The folded record takes the version/time of the oldest retained record
	// (recs[trimEnd]) so MaterializeAt and ListVersions stay monotonic.
	folded := record{
		version:   recs[trimEnd].version,
		update:    merged,
		updatedAt: recs[trimEnd].updatedAt,
	}
	newRecs := make([]record, 0, keep)
	newRecs = append(newRecs, folded)
	newRecs = append(newRecs, recs[trimEnd+1:]...)
	deleted := len(recs) - len(newRecs)
	r.records = newRecs
	// Keep records sorted ascending (they already are).
	sort.Slice(r.records, func(i, j int) bool { return r.records[i].version < r.records[j].version })
	return deleted, nil
}

// Delete removes all data for room.
func (m *MemoryPersistence) Delete(ctx context.Context, room string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rooms, room)
	return nil
}

// SaveSnapshot stores state as a new labelled snapshot and returns its ID.
func (m *MemoryPersistence) SaveSnapshot(ctx context.Context, room, label string, state []byte) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(state) == 0 {
		return 0, ErrEmptySnapshot
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.getRoom(room)
	id := r.nextSnapID
	r.nextSnapID++
	r.snapVersions = append(r.snapVersions, labelledSnapshot{
		id:        id,
		label:     label,
		createdAt: time.Now().UTC(),
		state:     append([]byte(nil), state...),
	})
	return id, nil
}

// ListSnapshots returns snapshot metadata newest-first.
func (m *MemoryPersistence) ListSnapshots(ctx context.Context, room string) ([]SnapshotInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return []SnapshotInfo{}, nil
	}
	out := make([]SnapshotInfo, 0, len(r.snapVersions))
	for i := len(r.snapVersions) - 1; i >= 0; i-- { // newest first
		s := r.snapVersions[i]
		out = append(out, SnapshotInfo{
			ID:        s.id,
			Label:     s.label,
			CreatedAt: s.createdAt,
			Size:      int64(len(s.state)),
		})
	}
	return out, nil
}

// GetSnapshotState returns one snapshot's state blob.
func (m *MemoryPersistence) GetSnapshotState(ctx context.Context, room string, id int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return nil, ErrSnapshotNotFound
	}
	for _, s := range r.snapVersions {
		if s.id == id {
			return append([]byte(nil), s.state...), nil
		}
	}
	return nil, ErrSnapshotNotFound
}

// DeleteSnapshot removes one snapshot. Deleting an unknown snapshot is a no-op.
func (m *MemoryPersistence) DeleteSnapshot(ctx context.Context, room string, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[room]
	if !ok {
		return nil
	}
	for i, s := range r.snapVersions {
		if s.id == id {
			r.snapVersions = append(r.snapVersions[:i], r.snapVersions[i+1:]...)
			return nil
		}
	}
	return nil
}

// ListRooms returns every room holding at least one update or snapshot.
func (m *MemoryPersistence) ListRooms(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.rooms))
	for name, r := range m.rooms {
		// Skip a room whose entry exists but holds nothing (e.g. pruned to empty):
		// the contract is "has persisted data", not "was ever touched".
		if len(r.records) == 0 && len(r.snapshots) == 0 && len(r.snapVersions) == 0 {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}
