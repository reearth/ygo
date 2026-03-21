package encoding

import (
	"strings"
	"unicode/utf8"
)

// ── RleByte ───────────────────────────────────────────────────────────────────

// RleByteEncoder run-length-encodes a sequence of bytes.
// Format: [value, VarUint(count-1)] pairs.  The LAST run's count is omitted
// (the decoder infers it from the end of stream).
type RleByteEncoder struct {
	enc   Encoder
	state byte
	count int
}

// Write appends v to the encoded sequence.
func (e *RleByteEncoder) Write(v byte) {
	if e.count > 0 && v == e.state {
		e.count++
	} else {
		if e.count > 0 {
			e.enc.WriteVarUint(uint64(e.count - 1))
		}
		e.enc.WriteUint8(v)
		e.state = v
		e.count = 1
	}
}

// Bytes returns the encoded bytes.  Call only once after all Write calls.
func (e *RleByteEncoder) Bytes() []byte {
	// The last run's count is intentionally NOT written (matches lib0 behaviour).
	return e.enc.Bytes()
}

// RleByteDecoder run-length-decodes a byte stream produced by RleByteEncoder.
type RleByteDecoder struct {
	dec   *Decoder
	state byte
	count int // -1 means "forever" (last run, end of buffer)
}

// NewRleByteDecoder returns a decoder for data produced by RleByteEncoder.
func NewRleByteDecoder(data []byte) *RleByteDecoder {
	return &RleByteDecoder{dec: NewDecoder(data)}
}

// Read returns the next decoded byte.
func (d *RleByteDecoder) Read() (byte, error) {
	if d.count == 0 {
		b, err := d.dec.ReadUint8()
		if err != nil {
			return 0, err
		}
		d.state = b
		if d.dec.HasContent() {
			cnt, err := d.dec.ReadVarUint()
			if err != nil {
				return 0, err
			}
			d.count = int(cnt) + 1
		} else {
			d.count = -1 // last run: read forever
		}
	}
	if d.count > 0 {
		d.count--
	}
	return d.state, nil
}

// ── UintOptRle ────────────────────────────────────────────────────────────────

// UintOptRleEncoder encodes unsigned integers using optional run-length encoding.
// Singles: VarInt(+v). Runs: VarInt(-v) + VarUint(count-2).
// Uses sign-magnitude VarInt so that -0 is distinct from +0.
type UintOptRleEncoder struct {
	enc   Encoder
	state uint64
	count int
}

// Write appends v to the encoded sequence.
func (e *UintOptRleEncoder) Write(v uint64) {
	if e.count > 0 && v == e.state {
		e.count++
	} else {
		if e.count > 0 {
			e.flushRun()
		}
		e.state = v
		e.count = 1
	}
}

func (e *UintOptRleEncoder) flushRun() {
	if e.count == 1 {
		e.enc.WriteVarInt(int64(e.state))
	} else {
		// write -(state) — must use sign-magnitude so -0 is distinct from +0
		e.enc.writeNegVarUint(e.state)
		e.enc.WriteVarUint(uint64(e.count - 2))
	}
}

// Bytes returns the encoded bytes.  Call only once after all Write calls.
func (e *UintOptRleEncoder) Bytes() []byte {
	if e.count == 0 {
		return e.enc.Bytes()
	}
	// Copy accumulated bytes and flush the last run.
	var out Encoder
	out.buf = append(out.buf, e.enc.Bytes()...)
	if e.count == 1 {
		out.WriteVarInt(int64(e.state))
	} else {
		out.writeNegVarUint(e.state)
		out.WriteVarUint(uint64(e.count - 2))
	}
	return out.Bytes()
}

// UintOptRleDecoder decodes a stream produced by UintOptRleEncoder.
type UintOptRleDecoder struct {
	dec   *Decoder
	state uint64
	count int
}

// NewUintOptRleDecoder returns a decoder for data from UintOptRleEncoder.
func NewUintOptRleDecoder(data []byte) *UintOptRleDecoder {
	return &UintOptRleDecoder{dec: NewDecoder(data)}
}

// Read returns the next decoded value.
func (d *UintOptRleDecoder) Read() (uint64, error) {
	if d.count == 0 {
		mag, neg, err := d.dec.readVarIntWithSign()
		if err != nil {
			return 0, err
		}
		d.state = mag
		d.count = 1
		if neg {
			// run encoding: negative sign → read count
			cnt, err := d.dec.ReadVarUint()
			if err != nil {
				return 0, err
			}
			d.count = int(cnt) + 2
		}
	}
	d.count--
	return d.state, nil
}

// ── IntDiffOptRle ─────────────────────────────────────────────────────────────

// IntDiffOptRleEncoder encodes signed integers using diff + optional RLE.
// Encodes the diff between successive values, with a run-length flag in the LSB.
// encodedDiff = diff*2 + (count==1 ? 0 : 1), written as VarInt.
// For runs (count > 1), also writes VarUint(count-2).
type IntDiffOptRleEncoder struct {
	enc   Encoder
	state int64
	diff  int64
	count int
}

// Write appends v to the encoded sequence.
func (e *IntDiffOptRleEncoder) Write(v int64) {
	newDiff := v - e.state
	if e.count > 0 && newDiff == e.diff {
		e.state = v
		e.count++
	} else {
		if e.count > 0 {
			e.flushRun()
		}
		e.count = 1
		e.diff = v - e.state
		e.state = v
	}
}

func (e *IntDiffOptRleEncoder) flushRun() {
	encoded := e.diff * 2
	if e.count > 1 {
		encoded++
	}
	e.enc.WriteVarInt(encoded)
	if e.count > 1 {
		e.enc.WriteVarUint(uint64(e.count - 2))
	}
}

// Bytes returns the encoded bytes.  Call only once after all Write calls.
func (e *IntDiffOptRleEncoder) Bytes() []byte {
	if e.count == 0 {
		return e.enc.Bytes()
	}
	var out Encoder
	out.buf = append(out.buf, e.enc.Bytes()...)
	encoded := e.diff * 2
	if e.count > 1 {
		encoded++
	}
	out.WriteVarInt(encoded)
	if e.count > 1 {
		out.WriteVarUint(uint64(e.count - 2))
	}
	return out.Bytes()
}

// IntDiffOptRleDecoder decodes a stream produced by IntDiffOptRleEncoder.
type IntDiffOptRleDecoder struct {
	dec   *Decoder
	state int64
	diff  int64
	count int
}

// NewIntDiffOptRleDecoder returns a decoder for data from IntDiffOptRleEncoder.
func NewIntDiffOptRleDecoder(data []byte) *IntDiffOptRleDecoder {
	return &IntDiffOptRleDecoder{dec: NewDecoder(data)}
}

// Read returns the next decoded value.
func (d *IntDiffOptRleDecoder) Read() (int64, error) {
	if d.count == 0 {
		encodedDiff, err := d.dec.ReadVarInt()
		if err != nil {
			return 0, err
		}
		hasCount := encodedDiff & 1
		d.diff = encodedDiff >> 1 // arithmetic right shift
		d.count = 1
		if hasCount != 0 {
			cnt, err := d.dec.ReadVarUint()
			if err != nil {
				return 0, err
			}
			d.count = int(cnt) + 2
		}
	}
	d.state += d.diff
	d.count--
	return d.state, nil
}

// ── String ────────────────────────────────────────────────────────────────────

// StringEncoder concatenates strings and encodes their UTF-16 lengths via
// UintOptRle.  Format: VarString(all) + raw UintOptRle bytes (no length prefix).
type StringEncoder struct {
	strs []string
	lens UintOptRleEncoder
}

// Write appends s to the encoded string pool.
func (e *StringEncoder) Write(s string) {
	e.strs = append(e.strs, s)
	e.lens.Write(uint64(utf16CodeUnitLen(s)))
}

// Bytes returns the encoded bytes.  Call only once after all Write calls.
func (e *StringEncoder) Bytes() []byte {
	out := NewEncoder()
	out.WriteVarString(strings.Join(e.strs, ""))
	out.buf = append(out.buf, e.lens.Bytes()...)
	return out.Bytes()
}

// StringDecoder reads strings from a pool encoded by StringEncoder.
type StringDecoder struct {
	str  string
	spos int // current position in UTF-16 code units
	lens *UintOptRleDecoder
}

// NewStringDecoder returns a decoder for data produced by StringEncoder.
func NewStringDecoder(data []byte) (*StringDecoder, error) {
	if len(data) == 0 {
		return &StringDecoder{lens: &UintOptRleDecoder{dec: NewDecoder(nil)}}, nil
	}
	dec := NewDecoder(data)
	str, err := dec.ReadVarString()
	if err != nil {
		return nil, err
	}
	// Remaining bytes are the UintOptRle-encoded lengths.
	remaining := make([]byte, len(data)-dec.pos)
	copy(remaining, data[dec.pos:])
	return &StringDecoder{
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
	length := int(l)
	byteStart := utf16ToByteOffset(d.str, d.spos)
	d.spos += length
	byteEnd := utf16ToByteOffset(d.str, d.spos)
	return d.str[byteStart:byteEnd], nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// utf16CodeUnitLen returns the number of UTF-16 code units for s.
func utf16CodeUnitLen(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16ToByteOffset converts a UTF-16 code unit position to the corresponding
// byte offset in the UTF-8 string s.
func utf16ToByteOffset(s string, utf16Pos int) int {
	bytePos := 0
	u16Counted := 0
	for u16Counted < utf16Pos && bytePos < len(s) {
		r, size := utf8.DecodeRuneInString(s[bytePos:])
		if r >= 0x10000 {
			u16Counted += 2
		} else {
			u16Counted++
		}
		bytePos += size
	}
	return bytePos
}
