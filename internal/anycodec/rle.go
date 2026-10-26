package anycodec

import (
	"math"
	"unicode/utf8"
)

// Decoder cursor values can be copied independently; buffers remain read-only.

// RleByteDecoder run-length-decodes a byte stream produced by RleByteEncoder.
type RleByteDecoder struct {
	dec   Decoder
	state byte
	count int // -1 means "forever" (last run, end of buffer)
}

// NewRleByteDecoder returns a decoder for data produced by RleByteEncoder.
func NewRleByteDecoder(data []byte) RleByteDecoder {
	return RleByteDecoder{dec: NewDecoder(data)}
}

// Read returns the next decoded byte.
func (d *RleByteDecoder) Read() (byte, error) { return ReadRleByte(&d.dec, &d.state, &d.count) }

// ReadRleByte shares the wire traversal with the public decoder facade.
func ReadRleByte(dec *Decoder, state *byte, count *int) (byte, error) {
	if *count == 0 {
		b, err := dec.ReadUint8()
		if err != nil {
			return 0, err
		}
		*state = b
		if dec.HasContent() {
			cnt, err := dec.ReadVarUint()
			if err != nil {
				return 0, err
			}
			if cnt > math.MaxInt32 {
				return 0, ErrOverflow
			}
			*count = int(cnt) + 1
		} else {
			*count = -1 // last run: read forever
		}
	}
	if *count > 0 {
		*count--
	}
	return *state, nil
}

// UintOptRleDecoder decodes a stream produced by UintOptRleEncoder.
type UintOptRleDecoder struct {
	dec   Decoder
	state uint64
	count int
}

// NewUintOptRleDecoder returns a decoder for data from UintOptRleEncoder.
func NewUintOptRleDecoder(data []byte) UintOptRleDecoder {
	return UintOptRleDecoder{dec: NewDecoder(data)}
}

// Read returns the next decoded value.
func (d *UintOptRleDecoder) Read() (uint64, error) { return ReadUintOptRle(&d.dec, &d.state, &d.count) }

// ReadUintOptRle shares the wire traversal with the public decoder facade.
func ReadUintOptRle(dec *Decoder, state *uint64, count *int) (uint64, error) {
	if *count == 0 {
		mag, neg, err := dec.ReadVarIntWithSign()
		if err != nil {
			return 0, err
		}
		*state = mag
		*count = 1
		if neg {
			// run encoding: negative sign → read count
			cnt, err := dec.ReadVarUint()
			if err != nil {
				return 0, err
			}
			if cnt > math.MaxInt32 {
				return 0, ErrOverflow
			}
			*count = int(cnt) + 2
		}
	}
	*count--
	return *state, nil
}

// IntDiffOptRleDecoder decodes a stream produced by IntDiffOptRleEncoder.
type IntDiffOptRleDecoder struct {
	dec   Decoder
	state int64
	diff  int64
	count int
}

// NewIntDiffOptRleDecoder returns a decoder for data from IntDiffOptRleEncoder.
func NewIntDiffOptRleDecoder(data []byte) IntDiffOptRleDecoder {
	return IntDiffOptRleDecoder{dec: NewDecoder(data)}
}

// Read returns the next decoded value.
func (d *IntDiffOptRleDecoder) Read() (int64, error) {
	return ReadIntDiffOptRle(&d.dec, &d.state, &d.diff, &d.count)
}

// ReadIntDiffOptRle shares the wire traversal with the public decoder facade.
func ReadIntDiffOptRle(dec *Decoder, state *int64, diff *int64, count *int) (int64, error) {
	if *count == 0 {
		encodedDiff, err := dec.ReadVarInt()
		if err != nil {
			return 0, err
		}
		hasCount := encodedDiff & 1
		*diff = encodedDiff >> 1 // arithmetic right shift
		*count = 1
		if hasCount != 0 {
			cnt, err := dec.ReadVarUint()
			if err != nil {
				return 0, err
			}
			if cnt > math.MaxInt32 {
				return 0, ErrOverflow
			}
			*count = int(cnt) + 2
		}
	}
	*state += *diff
	*count--
	return *state, nil
}

// StringDecoder reads strings from a pool encoded by StringEncoder.
type StringDecoder struct {
	str     string
	bytePos int // current byte offset into str; reads are sequential
	lens    UintOptRleDecoder
}

// NewStringDecoder returns a decoder for data produced by StringEncoder.
func NewStringDecoder(data []byte) (StringDecoder, error) {
	if len(data) == 0 {
		return StringDecoder{lens: NewUintOptRleDecoder(nil)}, nil
	}
	dec := NewDecoder(data)
	str, err := dec.ReadVarString()
	if err != nil {
		return StringDecoder{}, err
	}
	// Remaining bytes are the UintOptRle-encoded lengths.
	remaining := dec.RemainingBytesCopy()
	return StringDecoder{
		str:  str,
		lens: NewUintOptRleDecoder(remaining),
	}, nil
}

// Read returns the next decoded string.
func (d *StringDecoder) Read() (string, error) {
	l, err := d.lens.Read()
	if err != nil {
		return "", err
	}
	return ReadStringChunk(d.str, &d.bytePos, l), nil
}

// ReadStringChunk advances a pool cursor by UTF-16 units without copying data.
func ReadStringChunk(str string, bytePos *int, units uint64) string {
	start := *bytePos
	end := AdvanceUTF16(str, start, int(units))
	*bytePos = end
	return str[start:end]
}

// AdvanceUTF16 returns the byte offset reached by advancing `units` UTF-16 code
// units forward from byteStart in s. It scans only the requested span, so a
// sequential StringDecoder (which remembers its byte position) decodes an entire
// column in O(total length) instead of O(n^2).
func AdvanceUTF16(s string, byteStart, units int) int {
	bytePos := byteStart
	counted := 0
	for counted < units && bytePos < len(s) {
		r, size := utf8.DecodeRuneInString(s[bytePos:])
		if r >= 0x10000 {
			counted += 2
		} else {
			counted++
		}
		bytePos += size
	}
	return bytePos
}
