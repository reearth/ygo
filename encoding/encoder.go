package encoding

import (
	"encoding/binary"
	"math"
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

// WriteVarInt encodes a signed integer via ZigZag mapping before WriteVarUint,
// so that small negative numbers produce small byte sequences.
// ZigZag: (n << 1) ^ (n >> 63)  maps  0→0, -1→1, 1→2, -2→3, ...
func (e *Encoder) WriteVarInt(v int64) {
	e.WriteVarUint(uint64((v << 1) ^ (v >> 63)))
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
		for k, item := range val {
			e.WriteVarString(k)
			e.WriteAny(item)
		}
	default:
		e.WriteUint8(126) // unknown types encoded as null
	}
}
