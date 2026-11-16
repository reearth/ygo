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
// The scratch document is uncapped because, lacking prior state, a legal
// incremental update parks everything that depends on it.
func ValidateV1(update []byte) error {
	return crdt.ApplyUpdateV1(crdt.New(crdt.WithMaxPendingItems(math.MaxInt)), update, nil)
}
