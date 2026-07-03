package crdt

// MergeIDSets returns a fresh IDSet containing every range of every input
// (yjs mergeIdSets). Inputs are not mutated.
func MergeIDSets(sets ...*IDSet) *IDSet {
	out := NewIDSet()
	for _, s := range sets {
		if s == nil {
			continue
		}
		for c, r := range s.clients {
			for _, rg := range r.getIDs() {
				out.Add(c, rg.Clock, rg.Len)
			}
		}
	}
	return out
}

// ExcludeIDSet returns set minus exclude as a fresh IDSet (yjs diffIdSet). A
// nil set is treated as empty; a nil exclude excludes nothing.
func ExcludeIDSet(set, exclude *IDSet) *IDSet {
	if set == nil {
		return NewIDSet()
	}
	if exclude == nil {
		return MergeIDSets(set)
	}
	out := NewIDSet()
	for client, r := range set.clients {
		setRanges := r.getIDs()
		if len(setRanges) == 0 {
			continue
		}
		er, ok := exclude.clients[client]
		if !ok {
			for _, rg := range setRanges {
				out.Add(client, rg.Clock, rg.Len)
			}
			continue
		}
		exRanges := er.getIDs()
		i, j := 0, 0
		curr := setRanges[0]
		hasCurr := true
		for hasCurr && j < len(exRanges) {
			e := exRanges[j]
			switch {
			case curr.Clock+curr.Len <= e.Clock: // disjoint, curr first
				if curr.Len > 0 {
					out.Add(client, curr.Clock, curr.Len)
				}
				i++
				hasCurr = i < len(setRanges)
				if hasCurr {
					curr = setRanges[i]
				}
			case e.Clock+e.Len <= curr.Clock: // disjoint, exclude first
				j++
			case e.Clock <= curr.Clock: // exclude laps into curr from the left
				newClock := e.Clock + e.Len
				if end := curr.Clock + curr.Len; end > newClock {
					curr = IDRange{Clock: newClock, Len: end - newClock}
					j++
				} else { // fully covered
					i++
					hasCurr = i < len(setRanges)
					if hasCurr {
						curr = setRanges[i]
					}
				}
			default: // curr starts before exclude: emit the head, keep the tail
				head := e.Clock - curr.Clock
				out.Add(client, curr.Clock, head)
				rem := uint64(0)
				if curr.Len > e.Len+head {
					rem = curr.Len - e.Len - head
				}
				curr = IDRange{Clock: curr.Clock + head + e.Len, Len: rem}
				if curr.Len == 0 {
					i++
					hasCurr = i < len(setRanges)
					if hasCurr {
						curr = setRanges[i]
					}
				} else {
					j++
				}
			}
		}
		if hasCurr {
			if curr.Len > 0 {
				out.Add(client, curr.Clock, curr.Len)
			}
			for i++; i < len(setRanges); i++ {
				out.Add(client, setRanges[i].Clock, setRanges[i].Len)
			}
		}
	}
	return out
}

// IntersectIDSets returns the overlap of a and b as a fresh IDSet
// (yjs intersectSets). A nil operand has no ranges, so the result is empty.
func IntersectIDSets(a, b *IDSet) *IDSet {
	out := NewIDSet()
	if a == nil || b == nil {
		return out
	}
	for client, ar := range a.clients {
		br, ok := b.clients[client]
		if !ok {
			continue
		}
		aR, bR := ar.getIDs(), br.getIDs()
		for i, j := 0, 0; i < len(aR) && j < len(bR); {
			ra, rb := aR[i], bR[j]
			clock := max(ra.Clock, rb.Clock)
			endA, endB := ra.Clock+ra.Len, rb.Clock+rb.Len
			end := min(endA, endB)
			if end > clock {
				out.Add(client, clock, end-clock)
			}
			if endA < endB {
				i++
			} else {
				j++
			}
		}
	}
	return out
}

// IDSetFromIDMap projects an IDMap onto a plain IDSet, dropping attribution
// (yjs createIdSetFromIdMap). A nil m projects to an empty IDSet.
func IDSetFromIDMap(m *IDMap) *IDSet {
	out := NewIDSet()
	if m == nil {
		return out
	}
	for client, r := range m.clients {
		for _, rg := range r.getIDs() {
			out.Add(client, rg.Clock, rg.Len)
		}
	}
	return out
}
