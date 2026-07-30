package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// Load returns the merged state of all updates up to the room's clamped head
// (the checkpoint target if one exists, else the latest version). An empty room
// yields a zero LoadResult.
//
// Reads (Load, ListVersions, GetUpdate, MaterializeAt) are intentionally
// lock-free: they never take s.mu. Consistency against a concurrent mid-prune
// comes from the clampSQL correlated subquery, which resolves the checkpoint
// ceiling and the row filter within a single statement. Do not add s.mu here —
// it would needlessly serialize reads against writers.
//
// Reads that issue TWO statements (Load, MaterializeAt run clampedHead then
// mergedUpTo) wrap both in a single read-only transaction: WAL snapshot
// isolation makes them see one consistent committed state, so a concurrent
// PruneAfter cannot interleave between the two statements and either expose
// future versions or return a mismatched {Version, Update}. This is still
// lock-free (no s.mu); the read tx only pins a snapshot, it does not block
// writers.
func (s *Store) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return persistence.LoadResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return persistence.LoadResult{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	head, err := s.clampedHead(ctx, tx, room)
	if err != nil {
		return persistence.LoadResult{}, err
	}
	if head == 0 {
		return persistence.LoadResult{}, nil
	}
	merged, err := s.mergedUpTo(ctx, tx, room, head)
	if err != nil {
		return persistence.LoadResult{}, err
	}
	return persistence.LoadResult{Update: merged, Version: head}, nil
}

// AppendUpdate validates and stores a V1 update as the room's next version,
// returning the assigned version. Invalid updates are rejected before any write.
func (s *Store) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recoverInterruptedPrune(ctx, room); err != nil {
		return 0, err
	}
	var maxV sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM updates WHERE room = ?`, room).Scan(&maxV); err != nil {
		return 0, err
	}
	next := persistence.Version(1)
	if maxV.Valid {
		next = persistence.Version(maxV.Int64) + 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO updates (room, version, data, created_at) VALUES (?, ?, ?, ?)`,
		room, int64(next), update, time.Now().UnixNano()); err != nil {
		return 0, err
	}
	return next, nil
}

// ListVersions returns metadata for every stored update in room with version
// <= the clamped head, newest first. An empty (or unknown) room yields nil.
func (s *Store) ListVersions(ctx context.Context, room string) ([]persistence.VersionMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, created_at FROM updates WHERE room = ? AND version <= `+clampSQL+
			` ORDER BY version DESC`, room, room)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.VersionMeta
	for rows.Next() {
		var v, ns int64
		if err := rows.Scan(&v, &ns); err != nil {
			return nil, err
		}
		out = append(out, persistence.VersionMeta{
			Version:   persistence.Version(v),
			UpdatedAt: time.Unix(0, ns),
		})
	}
	return out, rows.Err()
}

// GetUpdate returns the single (non-cumulative) update stored at version v and
// its metadata, with ok=true when present. Versions above the clamped head are
// invisible (ok=false, nil error), as are unknown rooms/versions.
func (s *Store) GetUpdate(ctx context.Context, room string, v persistence.Version) ([]byte, persistence.VersionMeta, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, persistence.VersionMeta{}, false, err
	}
	var data []byte
	var ns int64
	err := s.db.QueryRowContext(ctx,
		`SELECT data, created_at FROM updates WHERE room = ? AND version = ? AND version <= `+clampSQL,
		room, int64(v), room).Scan(&data, &ns)
	if err == sql.ErrNoRows {
		return nil, persistence.VersionMeta{}, false, nil
	}
	if err != nil {
		return nil, persistence.VersionMeta{}, false, err
	}
	return data, persistence.VersionMeta{Version: v, UpdatedAt: time.Unix(0, ns)}, true, nil
}

// MaterializeAt rebuilds the full V1 head state as of version v by merging all
// updates with version <= min(v, clamped head). It returns a nil slice for
// v == 0, and ErrRoomNotFound if the room has no visible updates and v > 0.
func (s *Store) MaterializeAt(ctx context.Context, room string, v persistence.Version) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v == 0 {
		return nil, nil
	}
	// Single read-only tx wrapping clampedHead + mergedUpTo (snapshot isolation),
	// for the same consistency reason as Load. See the Load doc comment.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	head, err := s.clampedHead(ctx, tx, room)
	if err != nil {
		return nil, err
	}
	if head == 0 {
		return nil, persistence.ErrRoomNotFound
	}
	upTo := v
	if head < upTo {
		upTo = head
	}
	return s.mergedUpTo(ctx, tx, room, upTo)
}

// CaptureSnapshot stores a named V1 snapshot for room, associated with the
// current clamped head version (0 for an empty room). An existing snapshot with
// the same (room, name) is overwritten.
func (s *Store) CaptureSnapshot(ctx context.Context, room, name string, state []byte) (persistence.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Under s.mu: no concurrent writer, so the bare handle is a consistent enough
	// read for the single clampedHead statement (no read tx needed).
	head, err := s.clampedHead(ctx, s.db, room)
	if err != nil {
		return 0, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO snapshots (room, name, version, state) VALUES (?, ?, ?, ?)
		 ON CONFLICT(room, name) DO UPDATE SET version=excluded.version, state=excluded.state`,
		room, name, int64(head), state)
	if err != nil {
		return 0, err
	}
	return head, nil
}

// RestoreSnapshot returns the V1 blob stored under (room, name), the version it
// was captured at, and ok=true when present. ok=false (nil error) when absent.
func (s *Store) RestoreSnapshot(ctx context.Context, room, name string) ([]byte, persistence.Version, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	var state []byte
	var v int64
	err := s.db.QueryRowContext(ctx,
		`SELECT state, version FROM snapshots WHERE room = ? AND name = ?`, room, name).Scan(&state, &v)
	if err == sql.ErrNoRows {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	return state, persistence.Version(v), true, nil
}

// Compact bounds a room's history to the newest keep updates, returning the
// number of rows removed. It never drops materialized state: the trimmed prefix
// is FOLDED into the oldest retained row (merged into one V1 blob), then the
// now-redundant older rows are deleted. Retained version numbers are unchanged,
// so reads still reconstruct the same head. keep <= 0 is a no-op (returns 0).
func (s *Store) Compact(ctx context.Context, room string, keep int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recoverInterruptedPrune(ctx, room); err != nil {
		return 0, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT version, data FROM updates WHERE room = ? AND version <= `+clampSQL+` ORDER BY version ASC`,
		room, room)
	if err != nil {
		return 0, err
	}
	var versions []int64
	var blobs [][]byte
	for rows.Next() {
		var v int64
		var b []byte
		if err := rows.Scan(&v, &b); err != nil {
			_ = rows.Close()
			return 0, err
		}
		versions = append(versions, v)
		blobs = append(blobs, b)
	}
	if cerr := rows.Close(); cerr != nil {
		return 0, cerr
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(versions) <= keep {
		return 0, nil
	}
	trimEnd := len(versions) - keep // fold [0:trimEnd] into versions[trimEnd]
	merged, err := crdt.MergeUpdatesV1(blobs[:trimEnd+1]...)
	if err != nil {
		return 0, err
	}
	foldVersion := versions[trimEnd]

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`UPDATE updates SET data = ? WHERE room = ? AND version = ?`, merged, room, foldVersion); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM updates WHERE room = ? AND version < ?`, room, foldVersion); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return trimEnd, nil
}

// Delete removes all data for room: its updates, named snapshots, and any
// crash-safety checkpoint, atomically in one transaction.
func (s *Store) Delete(ctx context.Context, room string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, q := range []string{
		`DELETE FROM updates WHERE room = ?`,
		`DELETE FROM snapshots WHERE room = ?`,
		`DELETE FROM snapshot_versions WHERE room = ?`,
		`DELETE FROM checkpoints WHERE room = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, room); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneAfter removes every update with version > target, crash-safely. It is
// two-phase: Phase 1 durably records a checkpoint (the prune ceiling plus the
// rolled-back head) so reads clamp to target the instant it commits; Phase 2
// deletes the future updates and clears the checkpoint. A crash between the two
// phases leaves stale future rows on disk, but the checkpoint keeps them
// invisible, and the next AppendUpdate finishes the interrupted prune. Mirrors
// persistence.FilePersistence.PruneAfter.
func (s *Store) PruneAfter(ctx context.Context, room string, target persistence.Version, rolledBack []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Phase 1: durably record the checkpoint (prune ceiling + rolled-back head).
	// rolledBack is stored for parity/forensics only; this backend reconstructs
	// head from surviving rows (<= target are never deleted) and never reads it
	// back. See the checkpoints schema comment in sqlite.go.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO checkpoints (room, target, rolled_back_head) VALUES (?, ?, ?)
		 ON CONFLICT(room) DO UPDATE SET target=excluded.target, rolled_back_head=excluded.rolled_back_head`,
		room, int64(target), rolledBack); err != nil {
		return err
	}

	// Simulated crash between checkpoint write and deletes (test-only).
	if s.crashAfterCheckpoint != nil && s.crashAfterCheckpoint() {
		return nil
	}

	// Phase 2: delete future updates, then clear the checkpoint.
	return s.finishPrune(ctx, room, target)
}

// SaveSnapshot stores state as a new labelled snapshot of room and returns its
// ID. AUTOINCREMENT guarantees the id is monotonic and never reused.
func (s *Store) SaveSnapshot(ctx context.Context, room, label string, state []byte) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(state) == 0 {
		return 0, persistence.ErrEmptySnapshot
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO snapshot_versions (room, label, created_at, state) VALUES (?, ?, ?, ?)`,
		room, label, time.Now().UnixNano(), state)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListSnapshots returns snapshot metadata for room, newest-first. The state blob
// is not read: only its length, so listing stays cheap for large snapshots.
func (s *Store) ListSnapshots(ctx context.Context, room string) ([]persistence.SnapshotInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label, created_at, length(state) FROM snapshot_versions
		 WHERE room = ? ORDER BY id DESC`, room)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []persistence.SnapshotInfo{}
	for rows.Next() {
		var id, ns, size int64
		var label string
		if err := rows.Scan(&id, &label, &ns, &size); err != nil {
			return nil, err
		}
		out = append(out, persistence.SnapshotInfo{
			ID:        id,
			Label:     label,
			CreatedAt: time.Unix(0, ns).UTC(),
			Size:      size,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetSnapshotState returns the state blob of one snapshot.
// persistence.ErrSnapshotNotFound when (room, id) is unknown.
func (s *Store) GetSnapshotState(ctx context.Context, room string, id int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var state []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT state FROM snapshot_versions WHERE room = ? AND id = ?`, room, id).Scan(&state)
	if err == sql.ErrNoRows {
		return nil, persistence.ErrSnapshotNotFound
	}
	if err != nil {
		return nil, err
	}
	return state, nil
}

// DeleteSnapshot removes one snapshot. Deleting an unknown snapshot is a no-op.
func (s *Store) DeleteSnapshot(ctx context.Context, room string, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM snapshot_versions WHERE room = ? AND id = ?`, room, id)
	return err
}

// ListRooms returns every room holding at least one update or snapshot.
func (s *Store) ListRooms(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT room FROM updates
		 UNION SELECT room FROM snapshots
		 UNION SELECT room FROM snapshot_versions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return nil, err
		}
		out = append(out, room)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
