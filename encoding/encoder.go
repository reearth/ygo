package encoding

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Encoder writes values into a growing byte buffer using the lib0 encoding format.
type Encoder struct {
	buf []byte
}

// NewEncoder returns a new Encoder with a small pre-allocated buffer.
func NewEncoder() *Encoder {
	return &Encoder{buf: make([]byte, 0, 64)}
}

// Bytes returns the encoded bytes accumulated so far.
func (e *Encoder) Bytes() []byte { return e.buf }

// Reset clears the buffer so the Encoder can be reused.
func (e *Encoder) Reset() { e.buf = e.buf[:0] }

// WriteRaw appends raw bytes to the encoder buffer without any length prefix.
func (e *Encoder) WriteRaw(b []byte) { e.buf = append(e.buf, b...) }

// WriteUint8 writes a single byte.
func (e *Encoder) WriteUint8(v uint8) {
	e.buf = append(e.buf, v)
}

// WriteVarUint encodes v using variable-length encoding: 7 data bits per byte,
// with the MSB as a continuation flag (1 = more bytes follow, 0 = last byte).
// Integers up to 2^53 are supported to match JavaScript's safe integer range.
func (e *Encoder) WriteVarUint(v uint64) {
	for v >= 0x80 {
		e.buf = append(e.buf, byte(v)|0x80)
		v >>= 7
	}
	e.buf = append(e.buf, byte(v))
}

// WriteVarInt encodes a signed integer using the lib0 sign-magnitude format,
// matching the JavaScript lib0 library's writeVarInt.
// The sign occupies bit 6 of the first byte; bits 0-5 hold the low 6 bits of
// the magnitude. Continuation bytes carry 7 data bits each (bit 7 = more).
func (e *Encoder) WriteVarInt(v int64) {
	sign := byte(0)
	var mag uint64
	if v < 0 {
		sign = 0x40
		// Special-case MinInt64: uint64(-math.MinInt64) overflows in Go's
		// two's complement arithmetic because +2^63 cannot fit in int64 (N-C4).
		if v == math.MinInt64 {
			mag = 1 << 63
		} else {
			mag = uint64(-v)
		}
	} else {
		mag = uint64(v)
	}
	if mag < 64 {
		e.buf = append(e.buf, sign|byte(mag))
		return
	}
	e.buf = append(e.buf, 0x80|sign|byte(mag&0x3F))
	mag >>= 6
	for mag >= 128 {
		e.buf = append(e.buf, 0x80|byte(mag&0x7F))
		mag >>= 7
	}
	e.buf = append(e.buf, byte(mag))
}

// WriteVarString encodes s as VarUint(byteLength) followed by raw UTF-8 bytes.
func (e *Encoder) WriteVarString(s string) {
	e.WriteVarUint(uint64(len(s)))
	e.buf = append(e.buf, s...)
}

// WriteVarBytes encodes b as VarUint(len) followed by raw bytes.
func (e *Encoder) WriteVarBytes(b []byte) {
	e.WriteVarUint(uint64(len(b)))
	e.buf = append(e.buf, b...)
}

// WriteFloat32 writes a 32-bit IEEE 754 float in little-endian byte order.
func (e *Encoder) WriteFloat32(v float32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	e.buf = append(e.buf, b[:]...)
}

// WriteFloat64 writes a 64-bit IEEE 754 float in little-endian byte order.
func (e *Encoder) WriteFloat64(v float64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	e.buf = append(e.buf, b[:]...)
}

// writeNegVarUint writes -(v) using sign-magnitude VarInt format.
// Unlike WriteVarInt(-int64(v)), this correctly encodes -0 (as 0x40) when v=0.
func (e *Encoder) writeNegVarUint(v uint64) {
	const sign = byte(0x40)
	if v < 64 {
		e.buf = append(e.buf, sign|byte(v))
		return
	}
	e.buf = append(e.buf, 0x80|sign|byte(v&0x3F))
	v >>= 6
	for v >= 128 {
		e.buf = append(e.buf, 0x80|byte(v&0x7F))
		v >>= 7
	}
	e.buf = append(e.buf, byte(v))
}

// WriteAny encodes an arbitrary value using lib0's tagged-union format.
// Supported Go types and their wire tags:
//
//	nil           → 126 (null)
//	bool          → 120 (true) / 121 (false)
//	int / int64   → 125 + VarInt
//	float32       → 124 + 4 bytes LE
//	float64       → 123 + 8 bytes LE
//	string        → 119 + VarString
//	[]byte        → 116 + VarBytes
//	[]any         → 117 + VarUint(len) + elements
//	map[string]any→ 118 + VarUint(len) + key-value pairs
func (e *Encoder) WriteAny(v any) {
	switch val := v.(type) {
	case nil:
		e.WriteUint8(126)
	case bool:
		if val {
			e.WriteUint8(120)
		} else {
			e.WriteUint8(121)
		}
	case int:
		e.WriteUint8(125)
		e.WriteVarInt(int64(val))
	case int64:
		e.WriteUint8(125)
		e.WriteVarInt(val)
	case float32:
		e.WriteUint8(124)
		e.WriteFloat32(val)
	case float64:
		e.WriteUint8(123)
		e.WriteFloat64(val)
	case string:
		e.WriteUint8(119)
		e.WriteVarString(val)
	case []byte:
		e.WriteUint8(116)
		e.WriteVarBytes(val)
	case []any:
		e.WriteUint8(117)
		e.WriteVarUint(uint64(len(val)))
		for _, item := range val {
			e.WriteAny(item)
		}
	case map[string]any:
		e.WriteUint8(118)
		e.WriteVarUint(uint64(len(val)))
		// Sort keys for deterministic encoding; Go map iteration is random (N-M3).
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			e.WriteVarString(k)
			e.WriteAny(val[k])
		}
	default:
		// Silently encoding unsupported types as null causes data loss. Panic
		// loudly so programming errors (channels, funcs, etc.) are caught
		// immediately rather than silently corrupting documents (N-M2).
		panic(fmt.Sprintf("encoding: unsupported type %T passed to WriteAny", v))
	}
}
