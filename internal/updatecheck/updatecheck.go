// Package updatecheck centralises the check run on an update the room has
// already applied: the bundled persistence adapters (memory, file, sqlite) run
// it before storing the update, and the websocket server's BroadcastUpdate,
// including the relay path, before fanning it out, so all of them accept
// exactly the same updates.
package updatecheck

import (
	"math"

	"github.com/reearth/ygo/crdt"
)

// ValidateV1 returns an error unless update decodes as a lib0 V1 update. It
// applies the update to an empty scratch document and discards it.
//
// The scratch document has no pending-queue cap, on parked items or on parked
// delete ranges. Decoded without the state it was made against, an incremental
// update parks every item that depends on that state, and every delete range
// that names it, so a cap would refuse a legal update the room has already
// applied: an update setting 100,001 keys on a map an earlier update created
// parks all of them, past crdt's default cap of 100,000. The cap guards a
// long-lived document's memory against a peer that keeps parking items or
// deletes; this document lives for one decode, and the decoder's per-update
// limits already bound what it can park.
func ValidateV1(update []byte) error {
	return crdt.ApplyUpdateV1(crdt.New(crdt.WithMaxPendingItems(math.MaxInt)), update, nil)
}
