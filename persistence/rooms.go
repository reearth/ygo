package persistence

import "context"

// RoomLister is the optional room-enumeration extension of VersionedPersistence.
// A backend that implements it can report which rooms it holds data for, which
// is what an operator needs in order to run retention, cleanup, migration, or
// reconciliation across a whole store rather than one room at a time.
//
// Without it, a caller has to keep its own external index of room names and has
// no way to detect drift between that index and what is actually persisted.
//
// Method contract:
//
//   - ListRooms returns the name of every room the store holds data for, meaning
//     at least one stored update or at least one snapshot. Order is UNSPECIFIED:
//     sort the result if you need determinism. Returns an empty slice (not an
//     error) for an empty store.
//
// RoomLister deliberately does not embed VersionedPersistence, so adding it does
// not break existing third-party backends; implement it only if enumeration is
// cheap for the backend. Callers should type-assert for it.
//
// Enumeration may be O(rooms) and is not a hot path: treat it as an
// administrative operation, not something to call per request.
type RoomLister interface {
	ListRooms(ctx context.Context) ([]string, error)
}

// Compile-time assertions that the in-tree backends can enumerate rooms.
// (sqlite asserts the same in its own package.)
var (
	_ RoomLister = (*MemoryPersistence)(nil)
	_ RoomLister = (*FilePersistence)(nil)
)
