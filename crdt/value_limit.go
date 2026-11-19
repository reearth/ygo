package crdt

import (
	"fmt"

	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// ErrValueLimit is why DocContainsUpdateV1Limit, DiffUpdateV1Limit and
// HeldStructsDifferV1Limit stop decoding an update: it carries more values
// than the limit their caller passed. It wraps ErrInvalidUpdate.
var ErrValueLimit = fmt.Errorf("%w: it carries more values than the decode allows", ErrInvalidUpdate)

// valueLimit counts what a decode outside an apply decodes against the limit
// its caller passed: each struct, each delete range, each lib0 Any value at
// any depth, each JSON value and each element of an array or entry of a map
// inside one. It hangs on the scratch document the decode builds its structs
// on (Doc.values), and decodeStructsV1 and decodeContent count through it; a
// nil one counts nothing, which is every document but such a scratch one.
type valueLimit struct{ left int }

// valueLimitExceeded is the panic a valueLimit raises past its limit, inside
// the decode loop so that the value past it is refused before it is built;
// decodeLimitedV1 recovers it as ErrValueLimit.
type valueLimitExceeded struct{}

// values is d's value limit, nil for a nil d and for every document a decode
// outside an apply did not set one on.
func (d *Doc) values() *valueLimit {
	if d == nil {
		return nil
	}
	return d.valueLimit
}

// decode counts n values.
func (l *valueLimit) decode(n int) {
	if l == nil {
		return
	}
	l.left -= n
	if l.left < 0 {
		panic(valueLimitExceeded{})
	}
}

// value counts v, a value the decode built, and everything inside it.
func (l *valueLimit) value(v any) {
	if l == nil {
		return
	}
	l.decode(1)
	l.nested(v)
}

// nested counts what v holds, v itself counted already.
func (l *valueLimit) nested(v any) {
	if l == nil {
		return
	}
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			l.value(e)
		}
	case map[string]any:
		for _, e := range v {
			l.value(e)
		}
	}
}

// anyRun counts the n lib0 Any values dec holds next, and everything inside
// them, off the frame on a copy of the cursor, before any of them is built.
func (l *valueLimit) anyRun(dec *encoding.Decoder, n uint64) {
	if l == nil {
		return
	}
	scan := *dec
	for range n {
		if !l.anyValue(&scan, 0) {
			return
		}
	}
}

// anyValue counts one lib0 Any value at depth and everything inside it, and
// reports whether it read the whole value; a value it cannot read is left to
// the decode, which refuses it.
func (l *valueLimit) anyValue(dec *encoding.Decoder, depth int) bool {
	if depth > anycodec.MaxDepth {
		return false
	}
	l.decode(1)
	tag, err := dec.ReadUint8()
	if err != nil {
		return false
	}
	switch tag {
	case 127, 126, 120, 121: // undefined, null, true, false
		return true
	case 125:
		_, err = dec.ReadVarInt()
	case 124:
		_, err = dec.ReadFloat32()
	case 123:
		_, err = dec.ReadFloat64()
	case 122:
		_, err = dec.ReadBigInt64()
	case 119, 116: // string, binary
		_, err = dec.ReadVarBytes()
	case 117, 118: // array, map
		elements, err := dec.ReadVarUint()
		if err != nil || elements > anycodec.MaxElements || elements > uint64(dec.Remaining()) {
			return false
		}
		for range elements {
			if tag == 118 {
				if _, err := dec.ReadVarBytes(); err != nil {
					return false
				}
			}
			if !l.anyValue(dec, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
	return err == nil
}

// deleteRangeCount is how many ranges ds holds over all its clients.
func deleteRangeCount(ds DeleteSet) int {
	n := 0
	for _, ranges := range ds.clients {
		n += len(ranges)
	}
	return n
}

// decodeLimitedV1 is decodeStructsV1 on a scratch document whose decode stops
// with ErrValueLimit past values; values below zero bounds nothing.
func decodeLimitedV1(update []byte, values int) (structs map[ClientID][]*Item, ds DeleteSet, err error) {
	scratch := newScratchDoc()
	if values >= 0 {
		scratch.valueLimit = &valueLimit{left: values}
	}
	defer func() {
		if r := recover(); r != nil {
			if _, over := r.(valueLimitExceeded); !over {
				panic(r)
			}
			structs, ds, err = nil, DeleteSet{}, ErrValueLimit
		}
	}()
	return decodeStructsV1(scratch, update)
}
