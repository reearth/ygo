package crdt

import (
	"unicode/utf8"

	"github.com/reearth/ygo/encoding"
)

// pendingBudget distinguishes a large complete update from a genuinely oversized
// pending queue before the decoder materializes the rest of its items. The cheap
// path needs no scan. At the cap, a wire-only dependency pass follows clocks to a
// fixed point, without constructing items, content values, or shared types.
// References merely being present in the message is insufficient: their own
// dependencies must be reachable too (including same-client predecessors).
type pendingBudget struct {
	initial   StateVector
	update    []byte
	remaining int
	v2        bool
	checked   bool
}

func newPendingBudget(doc *Doc, initial StateVector, update []byte, v2 bool) pendingBudget {
	remaining := doc.maxPendingItemsLimit()
	if doc.store.pending != nil {
		remaining -= len(doc.store.pending.items)
	}
	return pendingBudget{initial: initial, update: update, remaining: remaining, v2: v2}
}

func (b *pendingBudget) check(count int) error {
	if b.checked || count < b.remaining {
		return nil
	}
	known := make(StateVector, len(b.initial))
	for client, clock := range b.initial {
		known[client] = clock
	}
	for first := true; ; first = false {
		s := newPendingScanner(b.update, b.v2)
		clients := s.uint()
		if clients > maxV2Items {
			return ErrInvalidUpdate
		}
		var total uint64
		pending, progressed := 0, false
		for i := uint64(0); i < clients && s.err == nil; i++ {
			n := s.uint()
			total += n
			if total > maxV2Items {
				return ErrInvalidUpdate
			}
			client, clock := s.client(), s.uint()
			existingEnd := known.Clock(client)
			if first {
				existingEnd = b.initial.Clock(client)
			}
			for j := uint64(0); j < n && s.err == nil; j++ {
				length, skip, deps, numDeps := s.item()
				end := clock + length
				if end < clock {
					return ErrInvalidUpdate
				}
				// A skip denotes clocks absent from this message. It advances
				// the wire cursor only and cannot satisfy a dependency.
				if !skip && end > existingEnd {
					ready := clock <= existingEnd
					for _, dep := range deps[:numDeps] {
						if dep.Clock >= known.Clock(dep.Client) {
							ready = false
						}
					}
					if ready {
						existingEnd = end
						if end > known.Clock(client) {
							known[client] = end
							progressed = true
						}
					} else {
						pending++
					}
				}
				clock = end
			}
		}
		if s.err != nil {
			return wrapUpdateErr(s.err)
		}
		if pending <= b.remaining {
			b.checked = true
			return nil
		}
		if !progressed {
			return ErrInvalidUpdate
		}
	}
}

// Only decoder cursors and scalar clocks survive a scan. V1 reads the caller's
// buffer; V2 uses its normal column decoder without building a key dictionary
// or decoding Any/JSON values into object trees.
type pendingScanner struct {
	rest *encoding.Decoder
	v2   *v2Decoder
	keys int
	err  error
}

func newPendingScanner(update []byte, v2 bool) *pendingScanner {
	s := &pendingScanner{}
	if v2 {
		s.v2, s.err = newV2Decoder(update)
		if s.err == nil {
			s.rest = s.v2.restDec
		}
	} else {
		s.rest = encoding.NewDecoder(update)
	}
	return s
}

func (s *pendingScanner) uint() uint64 {
	if s.err != nil {
		return 0
	}
	v, err := s.rest.ReadVarUint()
	s.err = err
	return v
}
func (s *pendingScanner) byte() byte {
	if s.err != nil {
		return 0
	}
	v, err := s.rest.ReadUint8()
	s.err = err
	return v
}
func (s *pendingScanner) client() ClientID {
	if s.v2 == nil {
		return ClientID(s.uint())
	}
	if s.err != nil {
		return 0
	}
	v, err := s.v2.readClient()
	s.err = err
	return v
}
func (s *pendingScanner) id(right bool) ID {
	if s.v2 == nil {
		return ID{Client: s.client(), Clock: s.uint()}
	}
	if s.err != nil {
		return ID{}
	}
	var id ID
	if right {
		id, s.err = s.v2.readRightID()
	} else {
		id, s.err = s.v2.readLeftID()
	}
	return id
}
func (s *pendingScanner) length() uint64 {
	if s.v2 == nil {
		return s.uint()
	}
	if s.err != nil {
		return 0
	}
	v, err := s.v2.readLen()
	s.err = err
	return uint64(v)
}
func (s *pendingScanner) bytes() {
	if s.err == nil {
		_, s.err = s.rest.ReadVarBytes()
	}
}
func (s *pendingScanner) any() {
	if s.err == nil {
		s.err = s.rest.SkipAny()
	}
}
func (s *pendingScanner) text() uint64 {
	if s.err != nil {
		return 0
	}
	var n uint64
	if s.v2 != nil {
		var str string
		str, s.err = s.v2.readString()
		for _, r := range str {
			n++
			if r > 0xffff {
				n++
			}
		}
	} else {
		var raw []byte
		raw, s.err = s.rest.ReadVarBytes()
		if s.err == nil && !utf8.Valid(raw) {
			s.err = encoding.ErrInvalidUTF8
		}
		for len(raw) > 0 {
			r, size := utf8.DecodeRune(raw)
			raw = raw[size:]
			n++
			if r > 0xffff {
				n++
			}
		}
	}
	return n
}
func (s *pendingScanner) key() {
	if s.v2 == nil {
		s.text()
		return
	}
	if s.err != nil {
		return
	}
	var index int64
	index, s.err = s.v2.keyClockDec.Read()
	if index < 0 {
		s.err = ErrInvalidUpdate
	}
	if s.err == nil && index >= int64(s.keys) {
		s.text()
		s.keys++
	}
}
func (s *pendingScanner) item() (length uint64, skip bool, deps [3]ID, numDeps int) {
	var info byte
	if s.err != nil {
		return
	}
	if s.v2 == nil {
		info = s.byte()
	} else {
		info, s.err = s.v2.readInfo()
	}
	tag := info & 0x1f
	if tag == 0 {
		return s.length(), false, deps, 0
	}
	if tag == 10 {
		return s.uint(), true, deps, 0
	}
	if info&flagHasOrigin != 0 {
		deps[numDeps] = s.id(false)
		numDeps++
	}
	if info&flagHasRightOrigin != 0 {
		deps[numDeps] = s.id(true)
		numDeps++
	}
	if numDeps == 0 {
		var named bool
		if s.v2 == nil {
			named = s.byte() == 1
		} else if s.err == nil {
			named, s.err = s.v2.readParentInfo()
		}
		if named {
			s.text()
		} else {
			deps[0] = s.id(false)
			numDeps++
		}
		if info&flagHasParentSub != 0 {
			s.text()
		}
	}
	return s.content(tag), false, deps, numDeps
}
func (s *pendingScanner) content(tag byte) uint64 {
	if s.err != nil {
		return 0
	}
	switch tag {
	case wireDeleted:
		return s.length()
	case wireJSON, wireAny:
		n := s.length()
		if (s.v2 != nil && n > maxV2Items) || (s.v2 == nil && n > uint64(s.rest.Remaining())) {
			s.err = ErrInvalidUpdate
			return 0
		}
		for i := uint64(0); i < n && s.err == nil; i++ {
			if tag == wireJSON && s.v2 != nil {
				s.text()
			} else {
				s.any()
			}
		}
		return n
	case wireBinary:
		s.bytes()
	case wireString:
		return s.text()
	case wireEmbed:
		if s.v2 == nil {
			s.text()
		} else {
			s.any()
		}
	case wireFormat:
		s.key()
		if s.v2 == nil {
			s.text()
		} else {
			s.any()
		}
	case wireType:
		var ref byte
		if s.v2 == nil {
			ref = s.byte()
		} else {
			ref, s.err = s.v2.readTypeRef()
		}
		if ref == 3 || ref == 5 {
			s.key()
		}
	case wireDoc:
		if s.v2 == nil {
			s.bytes()
		} else {
			s.text()
		}
		s.any()
	case wireMove:
		s.uint()
		s.uint()
		s.uint()
	default:
		s.err = ErrInvalidUpdate
	}
	return 1
}
