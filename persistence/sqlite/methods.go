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
func (s *Store) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return persistence.LoadResult{}, err
	}
	head, err := s.clampedHead(ctx, room)
	if err != nil {
		return persistence.LoadResult{}, err
	}
	if head == 0 {
		return persistence.LoadResult{}, nil
	}
	merged, err := s.mergedUpTo(ctx, room, head)
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, created_at FROM updates WHERE room = ? AND version <= `+clampSQL+
			` ORDER BY version DESC`, room, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	if v == 0 {
		return nil, nil
	}
	head, err := s.clampedHead(ctx, room)
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
	return s.mergedUpTo(ctx, room, upTo)
}
