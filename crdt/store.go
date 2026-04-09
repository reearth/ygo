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
//
// The binary search uses only the start Clock (a plain integer comparison) to
// avoid calling Content.Len() — which requires a utf8.RuneCountInString scan —
// inside the hot O(log n) predicate. A single Content.Len() call after the
// search verifies that id.Clock falls within the candidate item's range.
func (s *StructStore) Find(id ID) *Item {
	items := s.clients[id.Client]
	n := len(items)
	if n == 0 {
		return nil
	}
	// Find the last item whose start Clock is ≤ id.Clock.
	idx := sort.Search(n, func(i int) bool {
		return items[i].ID.Clock > id.Clock
	}) - 1
	if idx < 0 {
		return nil
	}
	item := items[idx]
	if item.ID.Clock+uint64(item.Content.Len()) > id.Clock {
		return item
	}
	return nil
}

// getItemCleanEnd returns the item ending at exactly (client, clock).
// If the item at that position spans past clock it is split so the returned
// item ends exactly at clock. Used when a new item's origin falls inside an
// existing multi-character item.
func (s *StructStore) getItemCleanEnd(txn *Transaction, client ClientID, clock uint64) *Item {
	item := s.Find(ID{Client: client, Clock: clock})
	if item == nil {
		return nil
	}
	end := item.ID.Clock + uint64(item.Content.Len()) - 1
	if end == clock {
		return item
	}
	// Guard against malformed updates where clock < item.ID.Clock: the
	// subtraction would underflow, producing a huge splitAt that causes a
	// panic in Splice (N-H2).
	if clock < item.ID.Clock {
		return item
	}
	// Split so the left half ends exactly at clock.
	splitAt := int(clock - item.ID.Clock + 1)
	splitItem(txn, item, splitAt)
	return item // item is now the left half, ending at clock
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

// NextClock returns the next available clock value for the given client.
func (s *StructStore) NextClock(client ClientID) uint64 {
	items := s.clients[client]
	if len(items) == 0 {
		return 0
	}
	last := items[len(items)-1]
	return last.ID.Clock + uint64(last.Content.Len())
}

// insertItem inserts item into the per-client slice at the correct clock position.
// Used when splitting an existing item to register the right half.
func (s *StructStore) insertItem(item *Item) {
	items := s.clients[item.ID.Client]
	pos := sort.Search(len(items), func(i int) bool {
		return items[i].ID.Clock >= item.ID.Clock
	})
	items = append(items, nil)
	copy(items[pos+1:], items[pos:])
	items[pos] = item
	s.clients[item.ID.Client] = items
}

// findParentForMapEntry scans all items in the store for one that belongs
// to a map-type parent (has a non-empty ParentSub and a non-nil Parent).
// Used as a fallback when an item's origin is a GC placeholder with no
// parent info (Yjs wire-format limitation). Returns the first matching
// parent found. If the document has multiple map types, this may return
// any of them — but for single-map-type documents (the common case for
// this bug), it returns the correct parent.
func findParentForMapEntry(s *StructStore) *abstractType {
	for _, items := range s.clients {
		for _, item := range items {
			if item.ParentSub != "" && item.Parent != nil {
				return item.Parent
			}
		}
	}
	return nil
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
