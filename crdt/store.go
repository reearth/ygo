package crdt

import "sort"

// StructStore holds all Items across all clients in the document.
// Items for each client are stored in a slice sorted by Clock (append-only).
// This structure enables O(log n) lookup by ID via binary search and O(1) append.
type StructStore struct {
	clients map[ClientID][]*Item
}

func newStructStore() *StructStore {
	return &StructStore{clients: make(map[ClientID][]*Item)}
}

// Append adds item to the store. Items must be appended in Clock order per client.
func (s *StructStore) Append(item *Item) {
	s.clients[item.ID.Client] = append(s.clients[item.ID.Client], item)
}

// Find returns the Item that contains the given ID, or nil if not found.
// An item with Clock c and length l contains IDs with clocks [c, c+l).
func (s *StructStore) Find(id ID) *Item {
	items := s.clients[id.Client]
	if len(items) == 0 {
		return nil
	}
	// Binary search for the item whose clock range contains id.Clock.
	idx := sort.Search(len(items), func(i int) bool {
		return items[i].ID.Clock+uint64(items[i].Content.Len()) > id.Clock
	})
	if idx < len(items) && items[idx].ID.Clock <= id.Clock {
		return items[idx]
	}
	return nil
}

// getItemCleanEnd returns the item ending at exactly (client, clock).
// If the item at that position has length > 1 it is split, and the right half
// is returned. Used during update decoding when a new item's origin falls
// inside an existing multi-character item.
func (s *StructStore) getItemCleanEnd(client ClientID, clock uint64) *Item {
	item := s.Find(ID{Client: client, Clock: clock})
	if item == nil {
		return nil
	}
	end := item.ID.Clock + uint64(item.Content.Len()) - 1
	if end == clock {
		return item
	}
	// Split the item so it ends exactly at clock.
	// The right half keeps the same store slot; the left half is the split point.
	// Full split logic is implemented in Phase 4; for now just return the item.
	return item
}

// StateVector computes the current state vector: for each client, the clock of
// the last item + its length (i.e. the next expected clock from that client).
func (s *StructStore) StateVector() StateVector {
	sv := make(StateVector, len(s.clients))
	for client, items := range s.clients {
		if len(items) > 0 {
			last := items[len(items)-1]
			sv[client] = last.ID.Clock + uint64(last.Content.Len())
		}
	}
	return sv
}

// IterateFrom calls fn for every Item whose ID is not yet in sv,
// visiting items in client order, then clock order.
func (s *StructStore) IterateFrom(sv StateVector, fn func(*Item)) {
	for client, items := range s.clients {
		start := sv.Clock(client)
		for _, item := range items {
			if item.ID.Clock >= start {
				fn(item)
			}
		}
	}
}
