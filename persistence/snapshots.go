package persistence

import (
	"context"
	"errors"
	"time"
)

// SnapshotInfo is the metadata of one stored snapshot. The state blob itself is
// fetched separately via GetSnapshotState so listing stays cheap for rooms with
// many large snapshots.
type SnapshotInfo struct {
	// ID is the store-assigned identifier, unique WITHIN A ROOM and
	// monotonically increasing there (higher ID = newer snapshot). It is never
	// reused within a room, even after the snapshot is deleted.
	//
	// IDs are NOT guaranteed globally unique: the same ID may name a different
	// snapshot in a different room (backends may count per-room), so always
	// carry the room alongside the ID and never key a cache on the ID alone.
	ID int64
	// Label is the optional caller-supplied name ("before-migration",
	// "v2 draft"). NOT unique, and may be empty: saving twice under the same
	// label creates two distinct snapshots rather than overwriting.
	Label string
	// CreatedAt is the store-stamped creation time (UTC).
	CreatedAt time.Time
	// Size is the state blob length in bytes.
	Size int64
}

// ErrSnapshotNotFound is returned by GetSnapshotState when the (room, id) pair
// does not exist.
var ErrSnapshotNotFound = errors.New("persistence: snapshot not found")

// ErrEmptySnapshot is returned by SaveSnapshot when state is empty: an empty
// document has no state worth versioning.
var ErrEmptySnapshot = errors.New("persistence: empty snapshot state")

// SnapshotStore is the optional labelled-snapshot extension of
// VersionedPersistence. A backend that implements it can capture labelled
// point-in-time snapshots of a room independent of the live update log: the log
// can be appended to, compacted, or pruned without touching stored snapshots,
// and snapshots can be deleted without touching the log.
//
// This is the primitive an application builds a user-facing "version history"
// on. Note the distinction from ListVersions, which enumerates the raw update
// log (one entry per stored update) and is a durability concern, not a
// user-facing one.
//
// Method contracts:
//
//   - SaveSnapshot stores state (a self-contained V1 update blob carrying the
//     full room state) as a NEW snapshot and returns its ID. Empty state is
//     rejected with ErrEmptySnapshot. label may be empty and need not be
//     unique; repeated saves never overwrite.
//
//   - ListSnapshots returns the metadata of every snapshot of room,
//     NEWEST FIRST (matching ListVersions' ordering). Returns an empty slice
//     (not an error) for an unknown room or a room with no snapshots.
//
//   - GetSnapshotState returns the state blob of one snapshot.
//     ErrSnapshotNotFound if the (room, id) pair is unknown.
//
//   - DeleteSnapshot removes one snapshot. Idempotent: deleting an unknown
//     snapshot is a no-op returning a nil error.
//
// SnapshotStore deliberately does not embed VersionedPersistence so the latter's
// contract stays frozen; backends implement both, and callers needing both can
// take a SnapshotVersionedPersistence.
//
// The older name-keyed CaptureSnapshot/RestoreSnapshot pair on
// VersionedPersistence is superseded by this interface for new code: it is
// keyed by name (so a repeat save overwrites), exposes no metadata, and cannot
// be enumerated or individually deleted.
type SnapshotStore interface {
	SaveSnapshot(ctx context.Context, room, label string, state []byte) (int64, error)
	ListSnapshots(ctx context.Context, room string) ([]SnapshotInfo, error)
	GetSnapshotState(ctx context.Context, room string, id int64) ([]byte, error)
	DeleteSnapshot(ctx context.Context, room string, id int64) error
}

// SnapshotVersionedPersistence is a backend providing both the live update log
// and labelled snapshots. The memory, file, and sqlite backends implement it.
type SnapshotVersionedPersistence interface {
	VersionedPersistence
	SnapshotStore
}

// Compile-time assertions that the in-tree backends satisfy the snapshot
// contract. (sqlite asserts the same in its own package.)
var (
	_ SnapshotVersionedPersistence = (*MemoryPersistence)(nil)
	_ SnapshotVersionedPersistence = (*FilePersistence)(nil)
)
