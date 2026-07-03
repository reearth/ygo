package crdt

import "sort"

// IDRange is a contiguous run of item IDs for a single client:
// clocks [Clock, Clock+Len). Part of the attribution API (issue #56); the
// yjs-v14 counterpart is IdRange in src/utils/ids.js.
type IDRange struct {
	Clock uint64
	Len   uint64
}

// idRanges is a lazily-normalized list of IDRange (yjs IdRanges): add appends
// (fast-path-merging with the last range), getIDs sorts + merges on demand.
type idRanges struct {
	sorted bool
	ids    []IDRange
}

// add appends a range, merging with the trailing range when adjacent.
func (r *idRanges) add(clock, length uint64) {
	if n := len(r.ids); n > 0 && r.ids[n-1].Clock+r.ids[n-1].Len == clock {
		r.ids[n-1].Len += length
		return
	}
	r.sorted = false
	r.ids = append(r.ids, IDRange{Clock: clock, Len: length})
}

// getIDs returns the sorted, merged, zero-free range list (yjs IdRanges.getIds).
func (r *idRanges) getIDs() []IDRange {
	if r.sorted {
		return r.ids
	}
	r.sorted = true
	ids := r.ids
	sort.Slice(ids, func(i, j int) bool { return ids[i].Clock < ids[j].Clock })
	// Merge in place: j is the write cursor, i the read cursor.
	var i, j int
	for i, j = 1, 1; i < len(ids); i++ {
		left := ids[j-1]
		right := ids[i]
		switch {
		case left.Clock+left.Len >= right.Clock:
			if end := right.Clock + right.Len; end > left.Clock+left.Len {
				ids[j-1] = IDRange{Clock: left.Clock, Len: end - left.Clock}
			}
		case left.Len == 0:
			ids[j-1] = right
		default:
			if j < i {
				ids[j] = right
			}
			j++
		}
	}
	if len(ids) > 0 {
		if ids[j-1].Len == 0 {
			ids = ids[:j-1]
		} else {
			ids = ids[:j]
		}
	}
	r.ids = ids
	return ids
}

// IDSet records which item IDs are in the set, as per-client sorted runs.
// It is the yjs-v14 IdSet (src/utils/ids.js) — structurally a DeleteSet, but a
// standalone public type: DeleteSet stays private to transaction internals.
//
// An IDSet is not goroutine-safe.
type IDSet struct {
	clients map[ClientID]*idRanges
}

// NewIDSet returns an empty IDSet.
func NewIDSet() *IDSet {
	return &IDSet{clients: make(map[ClientID]*idRanges)}
}

// Add records the run [clock, clock+length) for client. length 0 is a no-op.
func (s *IDSet) Add(client ClientID, clock, length uint64) {
	if length == 0 {
		return
	}
	r, ok := s.clients[client]
	if !ok {
		r = &idRanges{}
		s.clients[client] = r
	}
	r.add(clock, length)
}

// Has reports whether (client, clock) is in the set.
func (s *IDSet) Has(client ClientID, clock uint64) bool {
	r, ok := s.clients[client]
	if !ok {
		return false
	}
	for _, rg := range r.getIDs() {
		if rg.Clock <= clock && clock < rg.Clock+rg.Len {
			return true
		}
		if rg.Clock > clock {
			break
		}
	}
	return false
}

// HasID reports whether id is in the set.
func (s *IDSet) HasID(id ID) bool { return s.Has(id.Client, id.Clock) }

// IsEmpty reports whether the set contains no ranges.
func (s *IDSet) IsEmpty() bool {
	for _, r := range s.clients {
		if len(r.getIDs()) > 0 {
			return false
		}
	}
	return true
}

// Clients returns the clients that own at least one range, ascending.
func (s *IDSet) Clients() []ClientID {
	out := make([]ClientID, 0, len(s.clients))
	for c, r := range s.clients {
		if len(r.getIDs()) > 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Ranges returns the normalized ranges for client (a copy), or nil.
func (s *IDSet) Ranges(client ClientID) []IDRange {
	r, ok := s.clients[client]
	if !ok {
		return nil
	}
	ids := r.getIDs()
	if len(ids) == 0 {
		return nil
	}
	out := make([]IDRange, len(ids))
	copy(out, ids)
	return out
}

// Clone returns a deep copy.
func (s *IDSet) Clone() *IDSet {
	out := NewIDSet()
	for c, r := range s.clients {
		ids := r.getIDs()
		cp := make([]IDRange, len(ids))
		copy(cp, ids)
		out.clients[c] = &idRanges{sorted: true, ids: cp}
	}
	return out
}
