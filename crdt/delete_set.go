package crdt

import "sort"

// DeleteRange is a contiguous range of deleted clocks for a single client.
type DeleteRange struct {
	Clock uint64
	Len   uint64
}

// DeleteSet tracks which Items have been deleted, stored as sorted, non-overlapping
// ranges per client. This compact representation is what travels on the wire.
type DeleteSet struct {
	clients map[ClientID][]DeleteRange
}

func newDeleteSet() DeleteSet {
	return DeleteSet{clients: make(map[ClientID][]DeleteRange)}
}

// add records that item (client, clock) with the given length has been deleted.
// Adjacent ranges are merged eagerly to keep the set compact.
func (ds *DeleteSet) add(id ID, length int) {
	ranges := ds.clients[id.Client]
	if len(ranges) > 0 {
		last := &ranges[len(ranges)-1]
		if last.Clock+last.Len == id.Clock {
			last.Len += uint64(length)
			ds.clients[id.Client] = ranges
			return
		}
	}
	ds.clients[id.Client] = append(ranges, DeleteRange{
		Clock: id.Clock,
		Len:   uint64(length),
	})
}

// IsDeleted reports whether the item at the given ID has been marked deleted.
func (ds *DeleteSet) IsDeleted(id ID) bool {
	for _, r := range ds.clients[id.Client] {
		if r.Clock <= id.Clock && id.Clock < r.Clock+r.Len {
			return true
		}
	}
	return false
}

// Merge incorporates all ranges from other into ds.
func (ds *DeleteSet) Merge(other DeleteSet) {
	for client, ranges := range other.clients {
		ds.clients[client] = append(ds.clients[client], ranges...)
	}
	// Re-sort and compact all affected clients.
	for client := range other.clients {
		ds.sortAndCompact(client)
	}
}

func (ds *DeleteSet) sortAndCompact(client ClientID) {
	ranges := ds.clients[client]
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Clock < ranges[j].Clock
	})
	compacted := ranges[:0]
	for _, r := range ranges {
		if len(compacted) > 0 {
			last := &compacted[len(compacted)-1]
			if last.Clock+last.Len >= r.Clock {
				end := r.Clock + r.Len
				if end > last.Clock+last.Len {
					last.Len = end - last.Clock
				}
				continue
			}
		}
		compacted = append(compacted, r)
	}
	ds.clients[client] = compacted
}

// Clients returns the client IDs that have at least one deleted range in ds.
func (ds *DeleteSet) Clients() []ClientID {
	out := make([]ClientID, 0, len(ds.clients))
	for c := range ds.clients {
		out = append(out, c)
	}
	return out
}

// applyTo marks items in the document as deleted according to the ranges in ds.
// Called when applying a remote update that carries a delete set.
func (ds *DeleteSet) applyTo(txn *Transaction) {
	for client, ranges := range ds.clients {
		items := txn.doc.store.clients[client]
		for _, r := range ranges {
			for _, item := range items {
				if item.ID.Clock >= r.Clock && item.ID.Clock < r.Clock+r.Len {
					item.delete(txn)
				}
			}
		}
	}
}
