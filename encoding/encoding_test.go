package encoding_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// roundtrip is a helper that encodes then immediately decodes a value,
// asserting the round-trip produces no error and returns the expected result.
func roundtripUint(t *testing.T, v uint64) {
	t.Helper()
	e := encoding.NewEncoder()
	e.WriteVarUint(v)
	got, err := encoding.NewDecoder(e.Bytes()).ReadVarUint()
	require.NoError(t, err)
	assert.Equal(t, v, got)
}

// --- VarUint ---

func TestUnit_VarUint_Boundaries(t *testing.T) {
	for _, v := range []uint64{
		0, 1, 127, 128, 255, 16383, 16384,
		math.MaxUint16, math.MaxUint32,
		1<<53 - 1, // max safe JS integer
	} {
		t.Run("", func(t *testing.T) { roundtripUint(t, v) })
	}
}

func TestUnit_VarUint_Sequential(t *testing.T) {
	vals := []uint64{0, 1, 128, 300, 16384, 100_000}
	e := encoding.NewEncoder()
	for _, v := range vals {
		e.WriteVarUint(v)
	}
	d := encoding.NewDecoder(e.Bytes())
	for _, want := range vals {
		got, err := d.ReadVarUint()
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assert.False(t, d.HasContent(), "buffer should be fully consumed")
}

func TestUnit_VarUint_OneByteRange(t *testing.T) {
	// Values 0-127 must encode to exactly 1 byte.
	for v := uint64(0); v <= 127; v++ {
		e := encoding.NewEncoder()
		e.WriteVarUint(v)
		assert.Len(t, e.Bytes(), 1, "v=%d should be 1 byte", v)
	}
}

// --- VarInt ---

func TestUnit_VarInt_RoundTrip(t *testing.T) {
	cases := []int64{
		0, 1, -1, 63, -64, 127, -128,
		math.MaxInt32, math.MinInt32,
		// lib0 sign-magnitude VarInt: magnitude fits in 55 bits.
		1<<52 - 1, -(1 << 52),
	}
	for _, v := range cases {
		e := encoding.NewEncoder()
		e.WriteVarInt(v)
		got, err := encoding.NewDecoder(e.Bytes()).ReadVarInt()
		require.NoError(t, err)
		assert.Equal(t, v, got, "value %d", v)
	}
}

func TestUnit_VarInt_SmallNegativesAreSmall(t *testing.T) {
	// -1 should encode to exactly 1 byte (sign-magnitude: 0x41).
	e := encoding.NewEncoder()
	e.WriteVarInt(-1)
	assert.Len(t, e.Bytes(), 1, "-1 should encode to a single byte")
}

// --- VarString ---

func TestUnit_VarString_RoundTrip(t *testing.T) {
	cases := []string{
		"",
		"hello",
		"こんにちは",       // multibyte UTF-8
		"😀🎉🚀",         // 4-byte codepoints
		"\x00nul\x00", // embedded null bytes
	}
	for _, s := range cases {
		e := encoding.NewEncoder()
		e.WriteVarString(s)
		got, err := encoding.NewDecoder(e.Bytes()).ReadVarString()
		require.NoError(t, err)
		assert.Equal(t, s, got)
	}
}

// --- VarBytes ---

func TestUnit_VarBytes_Empty(t *testing.T) {
	e := encoding.NewEncoder()
	e.WriteVarBytes([]byte{})
	got, err := encoding.NewDecoder(e.Bytes()).ReadVarBytes()
	require.NoError(t, err)
	assert.Equal(t, []byte{}, got)
}

func TestUnit_VarBytes_RoundTrip(t *testing.T) {
	b := []byte{0x00, 0x01, 0x7f, 0x80, 0xff}
	e := encoding.NewEncoder()
	e.WriteVarBytes(b)
	got, err := encoding.NewDecoder(e.Bytes()).ReadVarBytes()
	require.NoError(t, err)
	assert.Equal(t, b, got)
}

// --- Float32 / Float64 ---

func TestUnit_Float32_RoundTrip(t *testing.T) {
	cases := []float32{0, 1, -1, 3.14, math.MaxFloat32, float32(math.SmallestNonzeroFloat32)}
	for _, v := range cases {
		e := encoding.NewEncoder()
		e.WriteFloat32(v)
		got, err := encoding.NewDecoder(e.Bytes()).ReadFloat32()
		require.NoError(t, err)
		assert.Equal(t, math.Float32bits(v), math.Float32bits(got))
	}
}

func TestUnit_Float64_RoundTrip(t *testing.T) {
	cases := []float64{0, 1, -1, math.Pi, math.E, math.MaxFloat64, math.SmallestNonzeroFloat64}
	for _, v := range cases {
		e := encoding.NewEncoder()
		e.WriteFloat64(v)
		got, err := encoding.NewDecoder(e.Bytes()).ReadFloat64()
		require.NoError(t, err)
		assert.Equal(t, math.Float64bits(v), math.Float64bits(got))
	}
}

func TestUnit_Float64_NaN(t *testing.T) {
	e := encoding.NewEncoder()
	e.WriteFloat64(math.NaN())
	got, err := encoding.NewDecoder(e.Bytes()).ReadFloat64()
	require.NoError(t, err)
	assert.True(t, math.IsNaN(got))
}

// --- WriteAny / ReadAny ---

func TestUnit_Any_AllVariants(t *testing.T) {
	cases := []struct {
		name string
		val  any
	}{
		{"nil", nil},
		{"bool_true", true},
		{"bool_false", false},
		{"int_zero", int(0)},
		{"int_positive", int(42)},
		{"int_negative", int(-99)},
		{"float32", float32(1.5)},
		{"float64_pi", math.Pi},
		{"string_empty", ""},
		{"string_ascii", "hello world"},
		{"string_unicode", "日本語"},
		{"bytes_empty", []byte{}},
		{"bytes", []byte{0xde, 0xad, 0xbe, 0xef}},
		{"array_empty", []any{}},
		{"array_mixed", []any{int(1), "two", true, nil}},
		{"map_empty", map[string]any{}},
		{"map_basic", map[string]any{"key": "val", "n": int(7)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := encoding.NewEncoder()
			e.WriteAny(tc.val)
			got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
			require.NoError(t, err)
			assert.Equal(t, tc.val, got)
		})
	}
}

func TestUnit_Any_NestedStructure(t *testing.T) {
	v := map[string]any{
		"name":  "Alice",
		"score": int(100),
		"tags":  []any{"go", "crdt"},
		"meta":  map[string]any{"active": true},
	}
	e := encoding.NewEncoder()
	e.WriteAny(v)
	got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
	require.NoError(t, err)
	assert.Equal(t, v, got)
}

func TestUnit_Any_IntAlias(t *testing.T) {
	// WriteAny accepts plain int; ReadAny returns int.
	e := encoding.NewEncoder()
	e.WriteAny(int(42))
	got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
	require.NoError(t, err)
	assert.Equal(t, int(42), got)
}

// --- Encoder reset ---

func TestUnit_Encoder_Reset(t *testing.T) {
	e := encoding.NewEncoder()
	e.WriteVarUint(999)
	e.Reset()
	assert.Empty(t, e.Bytes())
	e.WriteVarUint(1)
	assert.Len(t, e.Bytes(), 1)
}

// --- Error conditions ---

func TestUnit_Decoder_TruncatedVarUint(t *testing.T) {
	// A byte with the continuation bit set but no following byte.
	d := encoding.NewDecoder([]byte{0x80})
	_, err := d.ReadVarUint()
	assert.ErrorIs(t, err, encoding.ErrUnexpectedEOF)
}

func TestUnit_Decoder_TruncatedVarBytes(t *testing.T) {
	// Claims 10 bytes but buffer only has 3.
	e := encoding.NewEncoder()
	e.WriteVarUint(10)
	e.WriteVarBytes([]byte{1, 2, 3}) // only 3 bytes of data
	raw := e.Bytes()[:4]             // cut off early
	_, err := encoding.NewDecoder(raw).ReadVarBytes()
	assert.ErrorIs(t, err, encoding.ErrUnexpectedEOF)
}

func TestUnit_Decoder_TruncatedFloat32(t *testing.T) {
	_, err := encoding.NewDecoder([]byte{0x01, 0x02}).ReadFloat32()
	assert.ErrorIs(t, err, encoding.ErrUnexpectedEOF)
}

func TestUnit_Decoder_TruncatedFloat64(t *testing.T) {
	_, err := encoding.NewDecoder([]byte{0x01, 0x02, 0x03, 0x04}).ReadFloat64()
	assert.ErrorIs(t, err, encoding.ErrUnexpectedEOF)
}

func TestUnit_Decoder_VarUintOverflow(t *testing.T) {
	// 8 continuation bytes → exceeds 53-bit guard.
	b := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00}
	_, err := encoding.NewDecoder(b).ReadVarUint()
	assert.ErrorIs(t, err, encoding.ErrOverflow)
}

func TestUnit_Decoder_EmptyBuffer(t *testing.T) {
	d := encoding.NewDecoder([]byte{})
	assert.False(t, d.HasContent())
	assert.Equal(t, 0, d.Remaining())
	_, err := d.ReadUint8()
	assert.ErrorIs(t, err, encoding.ErrUnexpectedEOF)
}

// ── Golden wire-format compatibility tests ────────────────────────────────────
//
// These tests pin the exact byte sequences produced by the lib0 JavaScript
// library (https://github.com/dmonad/lib0) for specific values. They catch any
// drift from the reference implementation before it reaches the wire.
//
// Byte values were derived directly from the encoding algorithms in encoder.go,
// which faithfully replicates the lib0 spec:
//   - VarUint: standard 7-bit continuation (LSB-first, bit 7 = more bytes).
//   - VarInt: lib0 sign-magnitude — sign in bit 6 of the first byte,
//             magnitude in bits 0-5 (first byte) then 7 bits per continuation byte.

// TestGolden_VarUint_KnownBytes verifies that specific unsigned integer values
// produce the exact byte sequences specified by lib0.
func TestGolden_VarUint_KnownBytes(t *testing.T) {
	cases := []struct {
		value uint64
		wire  []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{63, []byte{0x3F}},
		{127, []byte{0x7F}},
		// 128 = 0b10000000: low 7 bits = 0 with continuation, high = 1.
		{128, []byte{0x80, 0x01}},
		{255, []byte{0xFF, 0x01}},
		// 300 = 0b100101100: low7 = 44 (0x2C) with continuation = 0xAC, high = 2.
		{300, []byte{0xAC, 0x02}},
		// 16383 = 0b11111111111111: low7 = 127 with continuation = 0xFF, high = 127.
		{16383, []byte{0xFF, 0x7F}},
		// 16384 = 0b100000000000000: three bytes.
		{16384, []byte{0x80, 0x80, 0x01}},
		// Max safe JS integer (2^53 − 1): the overflow guard in the decoder
		// accepts exactly this value.
		{(1 << 53) - 1, func() []byte {
			e := encoding.NewEncoder()
			e.WriteVarUint((1 << 53) - 1)
			return e.Bytes()
		}()},
	}
	for _, tc := range cases {
		e := encoding.NewEncoder()
		e.WriteVarUint(tc.value)
		assert.Equal(t, tc.wire, e.Bytes(), "WriteVarUint(%d) wire mismatch", tc.value)

		got, err := encoding.NewDecoder(tc.wire).ReadVarUint()
		require.NoError(t, err, "ReadVarUint for value %d", tc.value)
		assert.Equal(t, tc.value, got, "ReadVarUint(%v) roundtrip", tc.wire)
	}
}

// TestGolden_VarInt_KnownBytes verifies the lib0 sign-magnitude VarInt format.
// Sign is stored in bit 6 (0x40) of the first byte; magnitude fills bits 0-5
// of the first byte and 7 bits of each continuation byte.
func TestGolden_VarInt_KnownBytes(t *testing.T) {
	cases := []struct {
		value int64
		wire  []byte
	}{
		{0, []byte{0x00}},
		// +1: sign=0, mag=1, mag<64 → single byte = 0|1 = 0x01
		{1, []byte{0x01}},
		// -1: sign=0x40, mag=1, mag<64 → single byte = 0x40|1 = 0x41
		{-1, []byte{0x41}},
		// +63: sign=0, mag=63, mag<64 → 0x3F
		{63, []byte{0x3F}},
		// -63: sign=0x40, mag=63, mag<64 → 0x40|63 = 0x7F
		{-63, []byte{0x7F}},
		// +64: sign=0, mag=64≥64 → first=0x80|0|byte(64&0x3F)=0x80, mag>>=6→1, second=0x01
		{64, []byte{0x80, 0x01}},
		// -64: sign=0x40, mag=64≥64 → first=0x80|0x40|0=0xC0, mag>>=6→1, second=0x01
		{-64, []byte{0xC0, 0x01}},
	}
	for _, tc := range cases {
		e := encoding.NewEncoder()
		e.WriteVarInt(tc.value)
		assert.Equal(t, tc.wire, e.Bytes(), "WriteVarInt(%d) wire mismatch", tc.value)
	}
}

// --- Fuzz ---

func FuzzDecodeVarUint(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0x7f})
	f.Add([]byte{0x80, 0x01})
	f.Add([]byte{0xff, 0xff, 0x03})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Must never panic regardless of input.
		d := encoding.NewDecoder(data)
		_, _ = d.ReadVarUint()
	})
}

func FuzzDecodeAny(f *testing.F) {
	// Seed with a valid encoded Any value.
	e := encoding.NewEncoder()
	e.WriteAny(map[string]any{"k": "v", "n": int64(1)})
	f.Add(e.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		d := encoding.NewDecoder(data)
		_, _ = d.ReadAny()
	})
}
