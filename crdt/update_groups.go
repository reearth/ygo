package crdt

import (
	"fmt"
	"sort"

	"github.com/reearth/ygo/encoding"
)

// groupSet marks wire groups by index; nil marks none.
type groupSet []uint64

func (s groupSet) has(i uint64) bool {
	w := i / 64
	return w < uint64(len(s)) && s[w]&(1<<(i%64)) != 0
}

// scanReason is why a scan stopped for err, as the decoder names it: the
// decoder error value that err is, unknownTagErr for a contentTagError, or
// ErrInvalidUpdate. It returns only package values or a new error, never its
// argument, so a scanner's decoder copies stay on the stack.
func scanReason(err error) error {
	if t, ok := err.(contentTagError); ok {
		return unknownTagErr(t&v2TagError != 0, byte(t))
	}
	for _, reason := range [...]error{encoding.ErrUnexpectedEOF, encoding.ErrOverflow, encoding.ErrUnknownTag, encoding.ErrInvalidUTF8, encoding.ErrDepthExceeded} {
		if err == reason {
			return reason
		}
	}
	return ErrInvalidUpdate
}

// unknownTagErr is the decoders' error for an unknown content tag.
func unknownTagErr(v2 bool, tag byte) error {
	if v2 {
		return fmt.Errorf("unknown V2 content tag: %d", tag)
	}
	return fmt.Errorf("unknown content tag: %d", tag)
}

// contentTagError is a scan's error for an unknown content tag: the tag, with
// v2TagError set in a V2 update. It holds no reference into the scanner.
type contentTagError uint16

const v2TagError contentTagError = 1 << 8

func tagError(tag byte, v2 bool) contentTagError {
	if v2 {
		return v2TagError | contentTagError(tag)
	}
	return contentTagError(tag)
}

func (t contentTagError) Error() string { return unknownTagErr(t&v2TagError != 0, byte(t)).Error() }

// supersededGroups marks wire groups that a later group for the same client
// replaces, and returns the bytes past the structs, where the delete set
// starts. Yjs keeps only the last group per client and never integrates the
// earlier ones. Encoders sort clients, so a strictly monotonic header sequence
// proves uniqueness without an index. v2 and rest are the update's initial V2
// decoder state (v2 nil for V1); both are copied, never advanced.
func supersededGroups(update []byte, v2 *v2Decoder, rest encoding.Decoder) (set groupSet, deleteSetBytes []byte, err error) {
	ordered, n, deleteSetBytes, err := scanGroupClients(update, v2, rest, nil)
	if err != nil {
		return nil, nil, err
	}
	if ordered {
		return nil, deleteSetBytes, nil
	}
	type entry struct {
		client ClientID
		index  uint32
	}
	entries := make([]entry, 0, n)
	if _, _, _, err := scanGroupClients(update, v2, rest, func(i uint64, client ClientID) {
		entries = append(entries, entry{client, uint32(i)})
	}); err != nil {
		return nil, nil, err
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
	return set, deleteSetBytes, nil
}

// scanGroupClients walks every group header and struct. It reports whether
// the clients are strictly monotonic, returns the bytes past the structs and
// calls visit, if set, per group. Its error holds no reference to the decoder
// copies.
func scanGroupClients(update []byte, v2 *v2Decoder, rest encoding.Decoder, visit func(uint64, ClientID)) (ordered bool, groups uint64, deleteSetBytes []byte, err error) {
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
		return false, 0, nil, ErrInvalidUpdate
	}
	ordered = true
	var previous ClientID
	direction := 0
	var total uint64
	for i := uint64(0); i < n && s.err == nil; i++ {
		items := s.uint()
		total += items
		if total > maxV2Items {
			return false, 0, nil, ErrInvalidUpdate
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
	if s.err != nil {
		return false, 0, nil, scanReason(s.err)
	}
	return ordered, n, s.rest.RemainingBytes(), nil
}
