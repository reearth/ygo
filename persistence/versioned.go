// Package persistence provides a versioned, append-only store for ygo CRDT
// documents, keyed by room name. Each committed transaction's incremental V1
// update is appended as a distinct Version; the head state can be loaded,
// rebuilt at any past version, snapshotted by name, and pruned.
//
// # Internal format
//
// Everything is stored in lib0 V1 (the format produced by
// crdt.EncodeStateAsUpdateV1 and consumed by crdt.ApplyUpdateV1). V1 is the
// only format ygo can merge (crdt.MergeUpdatesV1 has no V2 sibling). Callers
// that need V2 at the edge convert with crdt.UpdateV1ToV2 / UpdateV2ToV1.
//
// # Versions
//
// A Version is a monotonically increasing per-room sequence number assigned by
// AppendUpdate. Versions are dense and never reused. ListVersions returns the
// single (non-cumulative) updates newest-first; MaterializeAt folds the updates
// up to and including a version into a head state via MergeUpdatesV1.
//
// # Crash safety
//
// PruneAfter implements snapshot-before-delete: it first persists a rolled-back
// head plus a checkpoint at the target version, and only then deletes the
// updates newer than the target. A crash between those steps must never
// resurrect a "future" version on reopen — implementations enforce the
// checkpoint as a hard ceiling on the visible version range.
package persistence

import (
	"context"
	"errors"
	"time"
)

// Version is a monotonically increasing per-room update sequence number,
// assigned by AppendUpdate starting at 1. Zero is the "no version" sentinel
// (an empty room).
type Version uint64

// VersionMeta describes one stored update without its payload.
type VersionMeta struct {
	Version   Version
	UpdatedAt time.Time
}

// LoadResult is the materialized head state of a room.
type LoadResult struct {
	// Update is the full lib0 V1 head state (suitable for crdt.ApplyUpdateV1).
	// Empty (nil) for a room that has no stored updates.
	Update []byte
	// Version is the highest version folded into Update, or 0 if the room is
	// empty.
	Version Version
}

// ErrRoomNotFound is returned by operations that require an existing room.
// Load returns a zero LoadResult (not this error) for an unknown room, matching
// the provider PersistenceAdapter contract where a missing room is (nil, nil).
var ErrRoomNotFound = errors.New("persistence: room not found")

// VersionedPersistence is an append-only, versioned CRDT store keyed by room.
//
// All methods take a context for cancellation; in-memory implementations honor
// only ctx cancellation at entry, while I/O-backed ones should respect it
// throughout. Implementations must be safe for concurrent use across rooms;
// per-room serialization of AppendUpdate is the implementation's responsibility.
type VersionedPersistence interface {
	// Load returns the materialized head state for room. For an unknown or
	// empty room it returns a zero LoadResult and a nil error.
	Load(ctx context.Context, room string) (LoadResult, error)

	// AppendUpdate appends one incremental V1 update to room's log and returns
	// the newly assigned Version. The update must be a valid V1 update; an
	// invalid update is rejected without advancing the version.
	AppendUpdate(ctx context.Context, room string, update []byte) (Version, error)

	// ListVersions returns metadata for every stored update in room,
	// newest-first. Each entry is a single (non-cumulative) update. Returns an
	// empty slice (not an error) for an unknown room.
	ListVersions(ctx context.Context, room string) ([]VersionMeta, error)

	// GetUpdate returns the single (non-cumulative) update stored at version v,
	// its metadata, and ok=true when present. ok=false (nil error) when the
	// room or version does not exist.
	GetUpdate(ctx context.Context, room string, v Version) (update []byte, meta VersionMeta, ok bool, err error)

	// MaterializeAt rebuilds the full V1 head state as of version v by merging
	// all updates with version <= v (crdt.MergeUpdatesV1). Returns an empty
	// slice for v == 0. Returns ErrRoomNotFound if room has no updates and
	// v > 0.
	MaterializeAt(ctx context.Context, room string, v Version) ([]byte, error)

	// CaptureSnapshot stores a named snapshot for room. state is a portable V1
	// blob (typically crdt.EncodeStateAsUpdateV1 of the materialized doc — NOT
	// a crdt.Snapshot state-vector marker). It returns the head Version the
	// snapshot is associated with (the current highest version, or 0 if empty).
	// A snapshot with the same (room, name) is overwritten.
	CaptureSnapshot(ctx context.Context, room, name string, state []byte) (Version, error)

	// RestoreSnapshot returns the V1 blob previously stored under (room, name),
	// the version it was captured at, and ok=true when present. ok=false (nil
	// error) when no such snapshot exists.
	RestoreSnapshot(ctx context.Context, room, name string) (update []byte, v Version, ok bool, err error)

	// PruneAfter removes every update with version > target, making target the
	// new head. rolledBack is the V1 head state at target (the caller supplies
	// it, typically from MaterializeAt(target)); it is persisted as a
	// crash-safe checkpoint BEFORE the deletes so that a crash mid-prune can
	// never expose a version > target on reopen. After PruneAfter, ListVersions
	// returns nothing newer than target.
	PruneAfter(ctx context.Context, room string, target Version, rolledBack []byte) error

	// Compact trims the oldest updates, keeping at most keep of the newest, by
	// folding the trimmed prefix into the oldest retained update so the
	// materialized state is preserved. Returns the number of update records
	// removed. keep <= 0 is treated as keep all (deleted = 0).
	Compact(ctx context.Context, room string, keep int) (deleted int, err error)

	// Delete removes all data for room (updates, snapshots, checkpoint).
	Delete(ctx context.Context, room string) error
}
