package crdt

import "github.com/reearth/ygo/encoding"

// checkBeforeApply refuses an update that would park past the pending cap
// before the apply integrates any of it, so a refused update changes nothing.
func (b *pendingBudget) checkBeforeApply(size int) error {
	if !b.mayParkPastRoom(size) {
		return nil
	}
	return b.check()
}

// firstPass runs the frame's first pass over the update; a whole pass keeps
// where the structs end and, when exact, the clocks they reach, so the
// delete-set check does not read the structs again.
func (b *pendingBudget) firstPass(known StateVector, clients *pendingClientRange, remaining int, whole bool) (pendingPass, error) {
	var result pendingPass
	var err error
	if b.v2 {
		v2 := *b.v2Start
		rest := b.v2Rest
		v2.restDec = &rest
		result, err = scanPendingGroupsV2(&v2, known, remaining, clients, b.superseded, b.structs, b.parked, whole)
	} else {
		result, err = scanPendingGroupsV1(b.update, known, remaining, clients, b.superseded, b.structs, b.parked, whole)
	}
	if err == nil && whole {
		b.deleteSetBytes, b.walked = result.rest, true
		if result.exact {
			// A copy, by hand, so that known stays on its callers' stacks.
			b.clocks = make(StateVector, len(known))
			for client, clock := range known {
				b.clocks[client] = clock
			}
		}
	}
	return result, err
}

// mayParkPastRoom is whether the update may park more items than the room left,
// by a bound that costs nothing to read.
func (b *pendingBudget) mayParkPastRoom(size int) bool {
	if !b.v2 && size/2 <= b.remaining {
		return false
	}
	if b.structs > 0 {
		return b.structs > b.remaining
	}
	if !b.v2 {
		return true
	}
	n, ok := v2OneGroupStructs(b.v2Rest)
	return !ok || n > uint64(b.remaining)
}

// v2OneGroupStructs is the struct count in the header of a V2 update's only
// group, from a copy of its rest cursor, or false when the header cannot give
// it: it is unreadable, or there are several groups, which only the group walk
// counts.
func v2OneGroupStructs(rest encoding.Decoder) (uint64, bool) {
	groups, err := rest.ReadVarUint()
	if err != nil || groups > 1 {
		return 0, false
	}
	if groups == 0 {
		return 0, true
	}
	structs, err := rest.ReadVarUint()
	return structs, err == nil
}
