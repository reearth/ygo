package crdt

import (
	"encoding/binary"
	"slices"
)

// pendingCollector retains a record per blocked group and the group's
// unresolved structs as a varint summary, so a blocked group costs about its
// wire size rather than a decoder copy. A sizing pass (fill false) only
// counts, letting the filling pass allocate exact capacities.
type pendingCollector struct {
	groups      []pendingGroup
	sum         []byte
	heads, size int
	fill        bool
}

func (c *pendingCollector) head(client ClientID, clock, end, remaining uint64) {
	c.heads++
	if c.fill {
		c.groups = append(c.groups, pendingGroup{client: client, clock: clock, end: end, at: uint32(len(c.sum)), remaining: uint32(remaining)})
	}
}

// A summary entry is a flag byte (skip bit, dependency count), the struct
// length, then each dependency's client and clock.
func (c *pendingCollector) put(skip bool, length uint64, deps []ID) {
	flag := byte(len(deps)) << 1
	if skip {
		flag = 1
		deps = nil
	}
	if !c.fill {
		c.size += 1 + uvarintLen(length)
		for _, dep := range deps {
			c.size += uvarintLen(uint64(dep.Client)) + uvarintLen(dep.Clock)
		}
		return
	}
	c.sum = append(c.sum, flag)
	c.sum = binary.AppendUvarint(c.sum, length)
	for _, dep := range deps {
		c.sum = binary.AppendUvarint(c.sum, uint64(dep.Client))
		c.sum = binary.AppendUvarint(c.sum, dep.Clock)
	}
}

func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func pendingEntry(sum []byte, at uint32) (skip bool, length uint64, deps [3]ID, numDeps int, next uint32) {
	p := int(at)
	flag := sum[p]
	p++
	length, n := binary.Uvarint(sum[p:])
	p += n
	numDeps = int(flag >> 1)
	for i := 0; i < numDeps; i++ {
		client, n := binary.Uvarint(sum[p:])
		p += n
		clock, n := binary.Uvarint(sum[p:])
		p += n
		deps[i] = ID{Client: ClientID(client), Clock: clock}
	}
	return flag&1 != 0, length, deps, numDeps, uint32(p)
}

// A group holds its current blocked struct. Surviving groups are unique per
// client, so only a group's own progress advances its client and each blocked
// group waits on at most one other group, in that group's pairing heap.
type pendingGroup struct {
	client                   ClientID
	clock, end               uint64
	wake                     uint64 // the awaited clock on the target's client
	at, remaining            uint32
	child, sibling, watchers int32
	done                     bool
}

func (g *pendingGroup) next(sum []byte) {
	for g.remaining > 0 {
		_, _, _, _, next := pendingEntry(sum, g.at)
		g.at = next
		skip, length, _, _, _ := pendingEntry(sum, g.at)
		g.clock, g.end = g.end, g.end+length
		g.remaining--
		if !skip {
			return
		}
	}
	g.done = true
}

func (g *pendingGroup) missing(sum []byte, known StateVector) (ID, bool) {
	_, _, deps, numDeps, _ := pendingEntry(sum, g.at)
	for _, dep := range deps[:numDeps] {
		if dep.Clock >= known.Clock(dep.Client) {
			return dep, true
		}
	}
	return ID{}, false
}

func pendingMeld(groups []pendingGroup, a, b int32) int32 {
	if a < 0 {
		return b
	}
	if b < 0 {
		return a
	}
	if groups[b].wake < groups[a].wake {
		a, b = b, a
	}
	groups[b].sibling = groups[a].child
	groups[a].child = b
	return a
}

// pendingPop removes root and returns the merged heap of its children.
func pendingPop(groups []pendingGroup, root int32) int32 {
	pairs := int32(-1)
	for c := groups[root].child; c >= 0; {
		a, b := c, groups[c].sibling
		c = -1
		if b >= 0 {
			c = groups[b].sibling
			groups[b].sibling = -1
		}
		groups[a].sibling = -1
		p := pendingMeld(groups, a, b)
		groups[p].sibling = pairs
		pairs = p
	}
	groups[root].child = -1
	merged := int32(-1)
	for pairs >= 0 {
		next := groups[pairs].sibling
		groups[pairs].sibling = -1
		merged = pendingMeld(groups, merged, pairs)
		pairs = next
	}
	return merged
}

// resolvePendingGroups advances groups to the update's fixed point. Queued
// groups are linked through sibling, which is free while outside a heap.
func resolvePendingGroups(groups []pendingGroup, sum []byte, known StateVector) {
	slices.SortFunc(groups, func(a, b pendingGroup) int {
		if a.client < b.client {
			return -1
		}
		if a.client > b.client {
			return 1
		}
		return 0
	})
	find := func(client ClientID) int32 {
		i, ok := slices.BinarySearchFunc(groups, client, func(g pendingGroup, c ClientID) int {
			if g.client < c {
				return -1
			}
			if g.client > c {
				return 1
			}
			return 0
		})
		if !ok {
			return -1
		}
		return int32(i)
	}
	top := int32(-1)
	for i := range groups {
		groups[i].child, groups[i].watchers = -1, -1
		groups[i].sibling = top
		top = int32(i)
	}
	for top >= 0 {
		index := top
		g := &groups[index]
		top, g.sibling = g.sibling, -1
		start := known.Clock(g.client)
		for !g.done {
			if g.end > known.Clock(g.client) {
				if g.clock > known.Clock(g.client) {
					break // Only this group could fill its own clock gap.
				}
				if dep, missing := g.missing(sum, known); missing {
					if target := find(dep.Client); target >= 0 && target != index {
						g.wake = dep.Clock
						groups[target].watchers = pendingMeld(groups, groups[target].watchers, index)
					}
					break
				}
				known[g.client] = g.end
			}
			g.next(sum)
		}
		// Watchers wait for clocks beyond the frontier, so only progress wakes them.
		frontier := known.Clock(g.client)
		for w := g.watchers; frontier > start && w >= 0 && groups[w].wake < frontier; w = g.watchers {
			g.watchers = pendingPop(groups, w)
			groups[w].sibling = top
			top = w
		}
	}
}
