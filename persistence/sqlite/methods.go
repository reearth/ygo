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
func (s *Store) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
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
