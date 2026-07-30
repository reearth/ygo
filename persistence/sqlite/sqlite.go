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
	"strings"
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
CREATE TABLE IF NOT EXISTS snapshot_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  room       TEXT    NOT NULL,
  label      TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  state      BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshot_versions_room ON snapshot_versions (room, id DESC);
CREATE TABLE IF NOT EXISTS checkpoints (
  room             TEXT    NOT NULL PRIMARY KEY,
  target           INTEGER NOT NULL,
  rolled_back_head BLOB
);`

// rolled_back_head is persisted for FilePersistence parity and forensics;
// this backend never reads it back. Head is reconstructed purely by
// merging surviving updates (rows <= target are never deleted by prune), so the
// column is intentionally write-only here. It is nullable on purpose: pruning to
// the empty head (target=0) materializes a nil rolled-back head, which is stored
// as SQL NULL. Compact preserves this invariant: it FOLDS a trimmed prefix into
// the oldest retained row (never dropping state), so head stays reconstructable
// from surviving rows.

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
	// synchronous=NORMAL + WAL: a committed checkpoint survives an app-level
	// crash (process kill), but a power/OS crash can lose the most recently
	// committed transaction. This is the accepted WAL durability tradeoff —
	// chosen for throughput; FULL would fsync on every commit.
	// A URI-form path may already carry a query string (e.g.
	// "file:data.db?cache=shared"); pick the separator accordingly so the DSN
	// never gets a second "?".
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(on)"

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

// querier is the read surface shared by *sql.DB and *sql.Tx, letting the read
// helpers run against either a bare handle (under s.mu, no concurrent writer) or
// a single read transaction (lock-free reads needing a consistent snapshot).
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) clampedHead(ctx context.Context, q querier, room string) (persistence.Version, error) {
	var v sql.NullInt64
	err := q.QueryRowContext(ctx,
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

func (s *Store) mergedUpTo(ctx context.Context, q querier, room string, upTo persistence.Version) ([]byte, error) {
	// Apply the checkpoint clamp directly here too (defense-in-depth): callers
	// pass an already-clamped upTo, but binding clampSQL keeps the SELECT correct
	// even if a checkpoint is committed between the head lookup and this query.
	rows, err := q.QueryContext(ctx,
		`SELECT data FROM updates WHERE room = ? AND version <= ? AND version <= `+clampSQL+
			` ORDER BY version ASC`,
		room, int64(upTo), room)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

// finishPrune deletes updates newer than target and clears the checkpoint, in
// one transaction. Snapshots are intentionally untouched (matches FilePersistence).
func (s *Store) finishPrune(ctx context.Context, room string, target persistence.Version) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM updates WHERE room = ? AND version > ?`, room, int64(target)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM checkpoints WHERE room = ?`, room); err != nil {
		return err
	}
	return tx.Commit()
}

// recoverInterruptedPrune finishes a prune that crashed after the checkpoint
// write but before the deletes. Called under s.mu by both AppendUpdate and
// Compact before they touch the updates table, so neither operates on rows a
// pending prune is about to remove.
func (s *Store) recoverInterruptedPrune(ctx context.Context, room string) error {
	var target sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT target FROM checkpoints WHERE room = ?`, room).Scan(&target)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return s.finishPrune(ctx, room, persistence.Version(target.Int64))
}

// SetCrashAfterCheckpoint satisfies persistence.CrashInjector (test-only).
func (s *Store) SetCrashAfterCheckpoint(fn func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crashAfterCheckpoint = fn
}

// Reopen satisfies persistence.Reopener: a fresh handle over the same backing
// store. In-memory stores (no durable file) return the same handle.
func (s *Store) Reopen() (persistence.VersionedPersistence, error) {
	if s.path == "" || s.path == ":memory:" {
		return s, nil
	}
	return Open(s.path)
}

// Compile-time assertions that *Store satisfies the persistence contract and the
// optional crash-safety hooks the conformance suite exercises.
var (
	_ persistence.VersionedPersistence         = (*Store)(nil)
	_ persistence.SnapshotStore                = (*Store)(nil)
	_ persistence.SnapshotVersionedPersistence = (*Store)(nil)
	_ persistence.CrashInjector                = (*Store)(nil)
	_ persistence.Reopener                     = (*Store)(nil)
)
