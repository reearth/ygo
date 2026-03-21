package encoding

import (
	"encoding/binary"
	"errors"
	"math"
)

var (
	// ErrUnexpectedEOF is returned when the buffer is exhausted before decoding completes.
	ErrUnexpectedEOF = errors.New("encoding: unexpected end of input")

	// ErrOverflow is returned when a VarUint exceeds the 53-bit safe integer range.
	// This matches JavaScript's Number.MAX_SAFE_INTEGER constraint in the Yjs protocol.
	ErrOverflow = errors.New("encoding: varuint overflow (> 53 bits)")
)

// Decoder reads values from a byte slice using the lib0 encoding format.
type Decoder struct {
	buf []byte
	pos int
}

// NewDecoder returns a Decoder that reads from b.
func NewDecoder(b []byte) *Decoder {
	return &Decoder{buf: b}
}

// Remaining returns the number of unread bytes.
func (d *Decoder) Remaining() int { return len(d.buf) - d.pos }

// HasContent reports whether there are unread bytes remaining.
func (d *Decoder) HasContent() bool { return d.pos < len(d.buf) }

// RemainingBytes returns a copy of the unread portion of the buffer.
func (d *Decoder) RemainingBytes() []byte {
	rem := d.buf[d.pos:]
	cp := make([]byte, len(rem))
	copy(cp, rem)
	return cp
}

func (d *Decoder) readByte() (byte, error) {
	if d.pos >= len(d.buf) {
		return 0, ErrUnexpectedEOF
	}
	b := d.buf[d.pos]
	d.pos++
	return b, nil
}

// ReadUint8 reads a single byte.
func (d *Decoder) ReadUint8() (uint8, error) {
	return d.readByte()
}

// ReadVarUint decodes a variable-length unsigned integer.
// Returns ErrOverflow if the value exceeds 53 significant bits.
func (d *Decoder) ReadVarUint() (uint64, error) {
	var result uint64
	var shift uint
	for {
		b, err := d.readByte()
		if err != nil {
			return 0, err
		}
		result |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, nil
		}
		shift += 7
		if shift > 49 {
			return 0, ErrOverflow
		}
	}
}

// ReadVarInt decodes a lib0 sign-magnitude variable-length integer.
// Returns ErrOverflow if the encoded magnitude exceeds 55 bits.
func (d *Decoder) ReadVarInt() (int64, error) {
	b, err := d.readByte()
	if err != nil {
		return 0, err
	}
	neg := b&0x40 != 0
	result := uint64(b & 0x3F)
	shift := uint(6)
	for b&0x80 != 0 {
		if shift > 48 {
			return 0, ErrOverflow
		}
		b, err = d.readByte()
		if err != nil {
			return 0, err
		}
		result |= uint64(b&0x7F) << shift
		shift += 7
	}
	if neg {
		return -int64(result), nil
	}
	return int64(result), nil
}

// ReadVarString decodes a length-prefixed UTF-8 string.
func (d *Decoder) ReadVarString() (string, error) {
	b, err := d.ReadVarBytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadVarBytes decodes a length-prefixed byte slice.
// The returned slice is a sub-slice of the decoder's buffer; copy if you need to retain it.
func (d *Decoder) ReadVarBytes() ([]byte, error) {
	n, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	end := d.pos + int(n)
	if end > len(d.buf) {
		return nil, ErrUnexpectedEOF
	}
	out := d.buf[d.pos:end]
	d.pos = end
	return out, nil
}

// ReadFloat32 reads a 32-bit little-endian IEEE 754 float.
func (d *Decoder) ReadFloat32() (float32, error) {
	if d.pos+4 > len(d.buf) {
		return 0, ErrUnexpectedEOF
	}
	bits := binary.LittleEndian.Uint32(d.buf[d.pos:])
	d.pos += 4
	return math.Float32frombits(bits), nil
}

// ReadFloat64 reads a 64-bit little-endian IEEE 754 float.
func (d *Decoder) ReadFloat64() (float64, error) {
	if d.pos+8 > len(d.buf) {
		return 0, ErrUnexpectedEOF
	}
	bits := binary.LittleEndian.Uint64(d.buf[d.pos:])
	d.pos += 8
	return math.Float64frombits(bits), nil
}

// readVarIntWithSign reads a sign-magnitude VarInt and returns the magnitude
// and whether the sign bit was set.  Used by UintOptRleDecoder to distinguish
// negative zero from positive zero.
func (d *Decoder) readVarIntWithSign() (magnitude uint64, negative bool, err error) {
	b, err := d.readByte()
	if err != nil {
		return 0, false, err
	}
	negative = b&0x40 != 0
	result := uint64(b & 0x3F)
	shift := uint(6)
	for b&0x80 != 0 {
		if shift > 48 {
			return 0, negative, ErrOverflow
		}
		b, err = d.readByte()
		if err != nil {
			return 0, negative, err
		}
		result |= uint64(b&0x7F) << shift
		shift += 7
	}
	return result, negative, nil
}

// ReadAny decodes a tagged-union value written by Encoder.WriteAny.
func (d *Decoder) ReadAny() (any, error) {
	tag, err := d.ReadUint8()
	if err != nil {
		return nil, err
	}
	switch tag {
	case 127, 126: // undefined, null
		return nil, nil
	case 120:
		return true, nil
	case 121:
		return false, nil
	case 125:
		v, err := d.ReadVarInt()
		if err != nil {
			return nil, err
		}
		return int(v), nil
	case 124:
		return d.ReadFloat32()
	case 123:
		return d.ReadFloat64()
	case 119:
		return d.ReadVarString()
	case 116:
		return d.ReadVarBytes()
	case 117:
		n, err := d.ReadVarUint()
		if err != nil {
			return nil, err
		}
		out := make([]any, n)
		for i := range out {
			if out[i], err = d.ReadAny(); err != nil {
				return nil, err
			}
		}
		return out, nil
	case 118:
		n, err := d.ReadVarUint()
		if err != nil {
			return nil, err
		}
		out := make(map[string]any, n)
		for range n {
			k, err := d.ReadVarString()
			if err != nil {
				return nil, err
			}
			if out[k], err = d.ReadAny(); err != nil {
				return nil, err
			}
		}
		return out, nil
	default:
		return nil, nil
	}
}
