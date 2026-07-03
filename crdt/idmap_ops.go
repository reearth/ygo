package crdt

// MergeIDMaps returns a fresh IDMap containing every attributed range of every
// input. Attributes are re-interned into the result so value-equal attrs from
// different inputs become one instance (yjs mergeIdMaps' attrMapper).
func MergeIDMaps(maps ...*IDMap) *IDMap {
	out := NewIDMap()
	for _, m := range maps {
		if m == nil {
			continue
		}
		for c, r := range m.clients {
			for _, rg := range r.getIDs() {
				out.Add(c, rg.Clock, rg.Len, rg.Attrs) // Add interns
			}
		}
	}
	return out
}

// ExcludeIDMap returns m minus the ranges in exclude, preserving attribution
// (yjs _diffSet applied to an IdMap). A nil m is treated as empty; a nil
// exclude excludes nothing.
func ExcludeIDMap(m *IDMap, exclude *IDSet) *IDMap {
	out := NewIDMap()
	if m == nil {
		return out
	}
	if exclude == nil {
		return MergeIDMaps(m)
	}
	for client, r := range m.clients {
		ranges := r.getIDs()
		er, ok := exclude.clients[client]
		if !ok {
			for _, rg := range ranges {
				out.Add(client, rg.Clock, rg.Len, rg.Attrs)
			}
			continue
		}
		exRanges := er.getIDs()
		for _, rg := range ranges {
			// Walk this attributed range against the exclusion ranges,
			// emitting the surviving sub-runs with the same attrs.
			clock, remaining := rg.Clock, rg.Len
			for _, e := range exRanges {
				if remaining == 0 {
					break
				}
				if e.Clock+e.Len <= clock {
					continue
				}
				if e.Clock >= clock+remaining {
					break
				}
				if e.Clock > clock { // emit the uncovered head
					out.Add(client, clock, e.Clock-clock, rg.Attrs)
				}
				covered := min(clock+remaining, e.Clock+e.Len)
				remaining = clock + remaining - covered
				clock = covered
			}
			if remaining > 0 {
				out.Add(client, clock, remaining, rg.Attrs)
			}
		}
	}
	return out
}

// IntersectIDMaps returns the overlap of a and b; each overlap range carries
// the concatenation of both sides' attrs (yjs _intersectSets on AttrRanges:
// concat, no value-dedup — preserved for wire fidelity). A nil operand has
// no ranges, so the result is empty.
func IntersectIDMaps(a, b *IDMap) *IDMap {
	out := NewIDMap()
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
				out.Add(client, clock, end-clock, append(append([]*ContentAttribute{}, ra.Attrs...), rb.Attrs...))
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

// FilterIDMap returns the ranges whose attrs satisfy pred (yjs filterIdMap).
// A nil m has no ranges, so the result is empty.
func FilterIDMap(m *IDMap, pred func([]*ContentAttribute) bool) *IDMap {
	out := NewIDMap()
	if m == nil {
		return out
	}
	for client, r := range m.clients {
		for _, rg := range r.getIDs() {
			if pred(rg.Attrs) {
				out.Add(client, rg.Clock, rg.Len, rg.Attrs)
			}
		}
	}
	return out
}

// IDMapFromIDSet stamps every range of s with attrs
// (yjs createIdMapFromIdSet). A nil s stamps to an empty IDMap.
func IDMapFromIDSet(s *IDSet, attrList []*ContentAttribute) *IDMap {
	out := NewIDMap()
	if s == nil {
		return out
	}
	for client, r := range s.clients {
		for _, rg := range r.getIDs() {
			out.Add(client, rg.Clock, rg.Len, attrList)
		}
	}
	return out
}
