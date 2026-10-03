// Package updatecheck centralises the check the bundled persistence adapters
// (memory, file, sqlite) run on an update before storing it, so all three
// accept exactly the same updates.
package updatecheck

import (
	"math"

	"github.com/reearth/ygo/crdt"
)

// ValidateV1 returns an error unless update decodes as a lib0 V1 update. It
// applies the update to an empty scratch document and discards it.
//
// The scratch document has no pending-queue cap. Decoded without the state it
// was made against, an incremental update parks every item that depends on
// that state, so a cap would refuse a legal update the room has already
// applied: an update setting 100,001 keys on a map an earlier update created
// parks all of them, past crdt's default cap of 100,000. The cap guards a
// long-lived document's memory against a peer that keeps parking items; this
// document lives for one decode, and the decoder's per-update item limit
// already bounds what it can park.
func ValidateV1(update []byte) error {
	return crdt.ApplyUpdateV1(crdt.New(crdt.WithMaxPendingItems(math.MaxInt)), update, nil)
}
