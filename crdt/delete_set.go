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

// applyToPartial applies delete-set entries whose target items are
// integrated, and returns a new DeleteSet containing entries whose
// target items are absent (or the uncovered suffix of a range whose
// earlier clocks are integrated but later clocks are not). The caller
// should merge the returned set into StructStore.pendingDs and retry
// it on subsequent applies.
//
// This is the pending-aware variant of applyTo, used by applyV1Txn
// and applyV2Txn to defer deletes that target not-yet-integrated items.
func (ds *DeleteSet) applyToPartial(txn *Transaction) DeleteSet {
	unresolvable := newDeleteSet()
	for client, ranges := range ds.clients {
		items := txn.doc.store.clients[client]
		if len(items) == 0 {
			// No items for this client — entire set of ranges is unresolvable.
			unresolvable.clients[client] = append(unresolvable.clients[client], ranges...)
			continue
		}
		for _, r := range ranges {
			// Binary search: first item whose end > r.Clock.
			lo := sort.Search(len(items), func(i int) bool {
				return items[i].ID.Clock+uint64(items[i].Content.Len()) > r.Clock
			})
			applied := uint64(0)
			for i := lo; i < len(items); i++ {
				item := items[i]
				if item.ID.Clock >= r.Clock+r.Len {
					break
				}
				// Compute overlap of [r.Clock, r.Clock+r.Len) with the item's span.
				end := r.Clock + r.Len
				itemEnd := item.ID.Clock + uint64(item.Content.Len())
				if itemEnd < end {
					end = itemEnd
				}
				item.delete(txn)
				applied = end - r.Clock
			}
			// Park the uncovered suffix of the range, if any.
			if applied < r.Len {
				unresolvable.clients[client] = append(unresolvable.clients[client], DeleteRange{
					Clock: r.Clock + applied,
					Len:   r.Len - applied,
				})
			}
		}
	}
	return unresolvable
}
