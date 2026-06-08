// Package sqlite provides a pure-Go (CGo-free) SQLite implementation of
// persistence.VersionedPersistence using modernc.org/sqlite. It stores per-room
// incremental V1 updates, named snapshots, and a crash-safety checkpoint in one
// database file (WAL mode). Reads clamp to the checkpoint; PruneAfter is
// two-phase and the next AppendUpdate finishes an interrupted prune, mirroring
// persistence.FilePersistence.
package sqlite

import (
	"context"
	"database/sql"
	"sync"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS updates (
  room       TEXT    NOT NULL,
  version    INTEGER NOT NULL,
  data       BLOB    NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (room, version)
);
CREATE TABLE IF NOT EXISTS snapshots (
  room    TEXT    NOT NULL,
  name    TEXT    NOT NULL,
  version INTEGER NOT NULL,
  state   BLOB    NOT NULL,
  PRIMARY KEY (room, name)
);
CREATE TABLE IF NOT EXISTS checkpoints (
  room             TEXT    NOT NULL PRIMARY KEY,
  target           INTEGER NOT NULL,
  rolled_back_head BLOB    NOT NULL
);`

// Store is a VersionedPersistence backed by a single SQLite database.
// The zero value is not usable; call Open.
type Store struct {
	db   *sql.DB
	path string // "" or ":memory:" => ephemeral in-memory

	mu sync.Mutex // serializes all writers

	crashAfterCheckpoint func() bool // test-only; see SetCrashAfterCheckpoint
}

// Open opens (creating if needed) a SQLite-backed Store at path. An empty path
// or ":memory:" opens an ephemeral in-memory database pinned to one connection.
func Open(path string) (*Store, error) {
	inMem := path == "" || path == ":memory:"
	dsn := path
	if inMem {
		dsn = ":memory:"
	}
	dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(on)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if inMem {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// clampSQL is the version ceiling for reads: the room's checkpoint target if
// present, else max int64. Used as a correlated subquery so a concurrent
// mid-prune cannot race the checkpoint-vs-rows lookup.
const clampSQL = `COALESCE((SELECT target FROM checkpoints WHERE room = ?), 9223372036854775807)`

func (s *Store) clampedHead(ctx context.Context, room string) (persistence.Version, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM updates WHERE room = ? AND version <= `+clampSQL,
		room, room).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return persistence.Version(v.Int64), nil
}

func (s *Store) mergedUpTo(ctx context.Context, room string, upTo persistence.Version) ([]byte, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT data FROM updates WHERE room = ? AND version <= ? ORDER BY version ASC`,
		room, int64(upTo))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blobs [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		blobs = append(blobs, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(blobs) == 0 {
		return nil, nil
	}
	return crdt.MergeUpdatesV1(blobs...)
}

// recoverInterruptedPrune finishes a prune that crashed after writing the
// checkpoint but before deleting future rows. Full body in Task 5; the
// no-checkpoint fast path is correct now.
func (s *Store) recoverInterruptedPrune(ctx context.Context, room string) error {
	var target sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT target FROM checkpoints WHERE room = ?`, room).Scan(&target); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	return nil // completed in Task 5
}
