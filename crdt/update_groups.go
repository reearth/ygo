package crdt

import (
	"sort"

	"github.com/reearth/ygo/encoding"
)

// groupSet marks wire groups by index; nil marks none.
type groupSet []uint64

func (s groupSet) has(i uint64) bool {
	w := i / 64
	return w < uint64(len(s)) && s[w]&(1<<(i%64)) != 0
}

// supersededGroups marks wire groups that a later group for the same client
// replaces, since Yjs integrates only the last, and counts the update's
// structs.
func supersededGroups(update []byte, v2 *v2Decoder, rest encoding.Decoder) (set groupSet, structs int, err error) {
	ordered, n, total, ok := scanGroupClients(update, v2, rest, nil)
	if !ok {
		return nil, 0, ErrInvalidUpdate
	}
	if ordered {
		return nil, int(total), nil
	}
	type entry struct {
		client ClientID
		index  uint32
	}
	entries := make([]entry, 0, n)
	if _, _, _, ok := scanGroupClients(update, v2, rest, func(i uint64, client ClientID) {
		entries = append(entries, entry{client, uint32(i)})
	}); !ok {
		return nil, 0, ErrInvalidUpdate
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].client != entries[j].client {
			return entries[i].client < entries[j].client
		}
		return entries[i].index < entries[j].index
	})
	for i := 0; i+1 < len(entries); i++ {
		if entries[i].client == entries[i+1].client {
			if set == nil {
				set = make(groupSet, (n+63)/64)
			}
			set[entries[i].index/64] |= 1 << (entries[i].index % 64)
		}
	}
	return set, int(total), nil
}

// scanGroupClients walks every group header and struct, reporting whether the
// clients are strictly monotonic and the struct count, and returns ok rather
// than an error to keep the decoder copies on the stack.
func scanGroupClients(update []byte, v2 *v2Decoder, rest encoding.Decoder, visit func(uint64, ClientID)) (ordered bool, groups, structs uint64, ok bool) {
	s := pendingScanner{frame: true}
	if v2 == nil {
		rest = *encoding.NewDecoder(update)
		s.rest = &rest
	} else {
		// Scanner methods read raw fields through s.rest only.
		dec := *v2
		dec.restDec = nil
		s.v2, s.rest = &dec, &rest
	}
	n := s.uint()
	if n > maxV2Items {
		return false, 0, 0, false
	}
	ordered = true
	var previous ClientID
	direction := 0
	var total uint64
	for i := uint64(0); i < n && s.err == nil; i++ {
		items := s.uint()
		total += items
		if total > maxV2Items {
			return false, 0, 0, false
		}
		client := s.client()
		s.uint()
		if visit != nil {
			visit(i, client)
		}
		if i > 0 && ordered {
			d := 1
			if client < previous {
				d = -1
			}
			if client == previous || direction != 0 && d != direction {
				ordered = false
			}
			direction = d
		}
		previous = client
		for j := uint64(0); j < items && s.err == nil; j++ {
			s.item()
		}
	}
	return ordered, n, total, s.err == nil
}
