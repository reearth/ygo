// Package sqlite provides a pure-Go (CGo-free) SQLite implementation of
// persistence.VersionedPersistence using modernc.org/sqlite. It stores per-room
// incremental V1 updates, named snapshots, and a crash-safety checkpoint in one
// database file (WAL mode). Reads clamp to the checkpoint; PruneAfter is
// two-phase and the next AppendUpdate finishes an interrupted prune, mirroring
// persistence.FilePersistence.
package sqlite

import (
	"database/sql"
	"sync"

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
