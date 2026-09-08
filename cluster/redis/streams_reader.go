package redis

import (
	"hash/fnv"
	"sort"
)

// readerFor hash-assigns a room to one of the Readers goroutines.
//
// Stable by construction: a room that migrated between readers across cycles
// would leave two readers holding cursors for it, and they would replay each
// other's entries.
func (r *Relay) readerFor(room string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(room))
	return int(h.Sum32() % uint32(r.scfg.readers)) //nolint:gosec // readers is validated > 0
}

// roomsForReader returns the rooms currently assigned to one reader, sorted so
// an XREAD's key order is deterministic (which makes test failures readable
// and makes a cursor map easy to align with a response).
func (r *Relay) roomsForReader(idx int) []string {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()

	var out []string
	for room, n := range r.streamRooms {
		if n > 0 && r.readerFor(room) == idx {
			out = append(out, room)
		}
	}
	sort.Strings(out)
	return out
}

// keyBatches splits keys into runs of at most max.
//
// XREAD takes N keys plus N IDs, so an unbounded key set builds an unbounded
// command. Readers bounds concurrency; this bounds command size, and the two
// are independent — 10k rooms across 4 readers is 2500 keys per reader.
func keyBatches(keys []string, max int) [][]string {
	if len(keys) == 0 {
		return nil
	}
	var out [][]string
	for i := 0; i < len(keys); i += max {
		end := i + max
		if end > len(keys) {
			end = len(keys)
		}
		out = append(out, keys[i:end])
	}
	return out
}
