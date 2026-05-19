package encoding

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// ErrVarIntOutOfRange is returned by WriteVarIntE when the magnitude of v
// exceeds the lib0 VarInt encoding range ((1<<55)-1). The JS Yjs decoder
// rejects values outside this range, so producing one would be unwire-able.
var ErrVarIntOutOfRange = errors.New("encoding: VarInt magnitude exceeds lib0 55-bit range")

// Encoder writes values into a growing byte buffer using the lib0 encoding format.
// Encoder is not safe for concurrent use; each goroutine should use its own instance.
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

// WriteVarIntE is the error-returning variant of WriteVarInt. Returns
// ErrVarIntOutOfRange if the magnitude of v exceeds the lib0 55-bit range.
// Preferred over WriteVarInt for callers who need to surface out-of-range
// inputs without a panic — for example, encoding pipelines that wrap user
// input or untrusted data.
//
// Successful encoding is byte-identical to WriteVarInt(v) for the same v.
func (e *Encoder) WriteVarIntE(v int64) error {
	sign := byte(0)
	var mag uint64
	if v < 0 {
		sign = 0x40
		mag = uint64(-v)
	} else {
		mag = uint64(v)
	}
	if mag > (1<<55)-1 {
		return ErrVarIntOutOfRange
	}
	if mag < 64 {
		e.buf = append(e.buf, sign|byte(mag))
		return nil
	}
	e.buf = append(e.buf, 0x80|sign|byte(mag&0x3F))
	mag >>= 6
	for mag >= 128 {
		e.buf = append(e.buf, 0x80|byte(mag&0x7F))
		mag >>= 7
	}
	e.buf = append(e.buf, byte(mag))
	return nil
}

// WriteVarInt encodes a signed integer using the lib0 sign-magnitude format,
// matching the JavaScript lib0 library's writeVarInt.
//
// Panics if the magnitude of v exceeds (1<<55)-1, which is the maximum
// encodable by the lib0 protocol (the decoder rejects anything larger).
// Most callers should prefer WriteVarIntE which returns this case as
// ErrVarIntOutOfRange instead of panicking.
func (e *Encoder) WriteVarInt(v int64) {
	if err := e.WriteVarIntE(v); err != nil {
		panic(fmt.Sprintf("encoding: WriteVarInt value %d exceeds 55-bit lib0 VarInt range", v))
	}
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

// BigInt represents lib0 writeAny tag 122, encoded as a signed 64-bit integer.
type BigInt int64

// WriteFloat32 writes a 32-bit IEEE 754 float in big-endian byte order.
func (e *Encoder) WriteFloat32(v float32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], math.Float32bits(v))
	e.buf = append(e.buf, b[:]...)
}

// WriteFloat64 writes a 64-bit IEEE 754 float in big-endian byte order.
func (e *Encoder) WriteFloat64(v float64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(v))
	e.buf = append(e.buf, b[:]...)
}

// WriteBigInt64 writes a signed 64-bit integer in big-endian byte order.
func (e *Encoder) WriteBigInt64(v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
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
//	nil            -> 126 (null)
//	bool           -> 120 (true) / 121 (false)
//	int / int64    -> 125 + VarInt
//	float32        -> 124 + 4 bytes BE
//	float64        -> 123 + 8 bytes BE
//	BigInt         -> 122 + 8 bytes BE
//	string         -> 119 + VarString
//	[]byte         -> 116 + VarBytes
//	[]any          -> 117 + VarUint(len) + elements
//	map[string]any -> 118 + VarUint(len) + key-value pairs
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
		e.writeAnyInt(int64(val))
	case int8:
		e.writeAnyInt(int64(val))
	case int16:
		e.writeAnyInt(int64(val))
	case int32:
		e.writeAnyInt(int64(val))
	case int64:
		e.writeAnyInt(val)
	case uint:
		e.writeAnyUint(uint64(val))
	case uint8:
		e.writeAnyInt(int64(val))
	case uint16:
		e.writeAnyInt(int64(val))
	case uint32:
		e.writeAnyInt(int64(val))
	case uint64:
		e.writeAnyUint(val)
	case float32:
		e.WriteUint8(124)
		e.WriteFloat32(val)
	case float64:
		// lib0 narrows float64 to float32 (tag 124) when the value round-trips
		// losslessly, matching lib0's isFloat32 dispatch.
		if isFloat32Lossless(val) {
			e.WriteUint8(124)
			e.WriteFloat32(float32(val))
		} else {
			e.WriteUint8(123)
			e.WriteFloat64(val)
		}
	case BigInt:
		e.WriteUint8(122)
		e.WriteBigInt64(int64(val))
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

// writeAnyInt dispatches an int64 to the correct lib0 Any tag based on
// magnitude, matching lib0's writeAny number dispatch (encoding.js):
//
//   - int32 range  → tag 125 (int) + VarInt: byte-identical to lib0 for
//     values JavaScript Number would treat as a small integer.
//   - float64 safe-int range → tag 123 (float64): values that exceed int32
//     but round-trip losslessly through float64's 52-bit mantissa. lib0
//     emits the same here because JS Number is float64.
//   - outside float64 safe-int → tag 122 (BigInt): Go's int64 can hold these
//     exactly; lib0 JS cannot (Number would lose precision), so the BigInt
//     tag is the correct cross-impl representation for full-precision int64.
//     Decodes as `bigint` on the JS side via lib0's BigInt path.
func (e *Encoder) writeAnyInt(v int64) {
	const (
		int32Min = -1 << 31
		int32Max = 1<<31 - 1
		safeMin  = -1 << 53 // -2^53; float64's lossless integer floor
		safeMax  = 1 << 53  // 2^53;  float64's lossless integer ceiling
	)
	switch {
	case v >= int32Min && v <= int32Max:
		e.WriteUint8(125)
		e.WriteVarInt(v)
	case v >= safeMin && v <= safeMax:
		e.WriteUint8(123)
		e.WriteFloat64(float64(v))
	default:
		e.WriteUint8(122)
		e.WriteBigInt64(v)
	}
}

// writeAnyUint dispatches a uint64. Values within int64 range route through
// writeAnyInt for the normal magnitude-based tag choice. Values exceeding
// int64 (> 2^63 - 1) can't be represented in BigInt's signed int64 wire
// format; they fall back to tag 123 (float64) with documented precision loss,
// matching lib0 JS's behavior for Numbers in that range.
func (e *Encoder) writeAnyUint(v uint64) {
	if v <= math.MaxInt64 {
		e.writeAnyInt(int64(v))
		return
	}
	e.WriteUint8(123)
	e.WriteFloat64(float64(v))
}

// isFloat32Lossless reports whether the float64 value round-trips through
// float32 exactly — i.e. lib0's isFloat32 check. Used by WriteAny to choose
// between tag 124 (float32, 4 bytes) and tag 123 (float64, 8 bytes).
//
// NaN deliberately returns false: `float64(float32(NaN)) != NaN` because
// NaN != NaN, so NaN routes to tag 123. lib0 has the same behavior.
func isFloat32Lossless(v float64) bool {
	return float64(float32(v)) == v
}
