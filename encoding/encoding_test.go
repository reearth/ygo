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
		{"int_zero", int64(0)},
		{"int_positive", int64(42)},
		{"int_negative", int64(-99)},
		{"float32", float32(1.5)},
		{"float64_pi", math.Pi},
		{"string_empty", ""},
		{"string_ascii", "hello world"},
		{"string_unicode", "日本語"},
		{"bytes_empty", []byte{}},
		{"bytes", []byte{0xde, 0xad, 0xbe, 0xef}},
		{"array_empty", []any{}},
		{"array_mixed", []any{int64(1), "two", true, nil}},
		{"map_empty", map[string]any{}},
		{"map_basic", map[string]any{"key": "val", "n": int64(7)}},
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
		"score": int64(100),
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
	// WriteAny accepts plain int; ReadAny returns int64 to preserve full precision.
	e := encoding.NewEncoder()
	e.WriteAny(int(42))
	got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
	require.NoError(t, err)
	assert.Equal(t, int64(42), got)
}

func TestGolden_Any_Lib0BigInt_KnownBytes(t *testing.T) {
	cases := []struct {
		name string
		val  encoding.BigInt
		wire []byte
	}{
		{
			name: "positive",
			val:  encoding.BigInt(42),
			wire: []byte{122, 0, 0, 0, 0, 0, 0, 0, 42},
		},
		{
			name: "negative",
			val:  encoding.BigInt(-42),
			wire: []byte{122, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xd6},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encoding.NewDecoder(tc.wire).ReadAny()
			require.NoError(t, err)
			assert.Equal(t, tc.val, got)

			e := encoding.NewEncoder()
			e.WriteAny(tc.val)
			assert.Equal(t, tc.wire, e.Bytes())
		})
	}
}

func TestGolden_Any_Lib0FloatBytes(t *testing.T) {
	t.Run("float32", func(t *testing.T) {
		wire := []byte{124, 0x3f, 0xc0, 0x00, 0x00}
		got, err := encoding.NewDecoder(wire).ReadAny()
		require.NoError(t, err)
		assert.InDelta(t, float32(1.5), got, 0)

		e := encoding.NewEncoder()
		e.WriteAny(float32(1.5))
		assert.Equal(t, wire, e.Bytes())
	})

	t.Run("float64", func(t *testing.T) {
		wire := []byte{123, 0x40, 0x09, 0x21, 0xfb, 0x54, 0x44, 0x2d, 0x18}
		got, err := encoding.NewDecoder(wire).ReadAny()
		require.NoError(t, err)
		assert.InDelta(t, math.Pi, got, 0)

		e := encoding.NewEncoder()
		e.WriteAny(math.Pi)
		assert.Equal(t, wire, e.Bytes())
	})
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

// --- Fix 1: WriteVarInt range enforcement ---

func TestUnit_WriteVarInt_ExceedsRange_Panics(t *testing.T) {
	assert.Panics(t, func() {
		e := encoding.NewEncoder()
		e.WriteVarInt(math.MinInt64)
	})
	assert.Panics(t, func() {
		e := encoding.NewEncoder()
		e.WriteVarInt(math.MinInt64 + 1)
	})
}

func TestUnit_WriteVarInt_MaxRange_RoundTrips(t *testing.T) {
	maxMag := int64((1 << 55) - 1)
	for _, v := range []int64{0, 1, -1, maxMag, -maxMag} {
		e := encoding.NewEncoder()
		e.WriteVarInt(v)
		d := encoding.NewDecoder(e.Bytes())
		got, err := d.ReadVarInt()
		require.NoError(t, err, "value %d", v)
		assert.Equal(t, v, got, "value %d", v)
	}
}

// --- Fix 2: RLE uint64→int overflow guard ---

func TestUnit_VarUint_MaxInt32_Boundary(t *testing.T) {
	// Ensure uint64 values above MaxInt32 are representable in VarUint
	e := encoding.NewEncoder()
	e.WriteVarUint(math.MaxInt32 + 1)
	d := encoding.NewDecoder(e.Bytes())
	v, err := d.ReadVarUint()
	require.NoError(t, err)
	assert.Equal(t, uint64(math.MaxInt32+1), v)
}

// --- Fix 3 (post-#77 update): ReadAny tag 125 returns int64 in int32 range ---

func TestUnit_Any_Int64Precision_WithinInt32Range_RoundTripsAsInt64(t *testing.T) {
	// Values within int32 range use tag 125 + VarInt and decode as int64.
	v := int64(123_456_789) // within int32 range
	e := encoding.NewEncoder()
	e.WriteAny(v)
	got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
	require.NoError(t, err)
	assert.Equal(t, v, got, "int32-range int round-trips as int64 via tag 125")
}

func TestUnit_Any_Int64Precision_WithinFloat64SafeRange_RoundTripsAsFloat64(t *testing.T) {
	// Per #77 (lib0 parity): ints outside int32 but within float64's lossless
	// integer range (≤ 2^53) emit tag 123 (float64) and decode as float64.
	// JS readers see the same value because JS Number is float64.
	v := int64(1) << 40 // outside int32, inside float64 safe-int
	e := encoding.NewEncoder()
	e.WriteAny(v)
	got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
	require.NoError(t, err)
	// Delta 0 means exact equality — 2^40 is exactly representable in float64,
	// so this is a precise check, not a fuzzy one. assert.InDelta over
	// assert.Equal here is a testifylint requirement (float-compare rule).
	gotFloat, ok := got.(float64)
	require.True(t, ok, "safe-int-range int must decode as float64")
	assert.InDelta(t, float64(v), gotFloat, 0,
		"safe-int-range int round-trips as float64 via tag 123 (matches lib0)")
}

func TestUnit_Any_Int64Precision_BeyondFloat64SafeRange_RoundTripsAsBigInt(t *testing.T) {
	// Per #77: ints outside float64's safe range (> 2^53) need BigInt to
	// preserve precision. ygo emits tag 122 here so Go's full int64 range
	// round-trips losslessly. JS readers receive a `bigint`.
	v := int64((1 << 55) - 1) // > 2^53
	e := encoding.NewEncoder()
	e.WriteAny(v)
	got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
	require.NoError(t, err)
	assert.Equal(t, encoding.BigInt(v), got,
		"beyond-safe-int int round-trips as BigInt via tag 122 (precision preserved)")
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

// --- WriteVarIntE (#26) ---

func TestEncoder_WriteVarIntE_ValidInputProducesSameBytesAsWriteVarInt(t *testing.T) {
	cases := []int64{0, 1, -1, 63, -63, 64, -64, 1 << 54, -(1 << 54)}
	for _, v := range cases {
		a := encoding.NewEncoder()
		a.WriteVarInt(v)
		b := encoding.NewEncoder()
		require.NoError(t, b.WriteVarIntE(v))
		assert.Equal(t, a.Bytes(), b.Bytes(), "WriteVarInt and WriteVarIntE must produce identical bytes for v=%d", v)
	}
}

func TestEncoder_WriteVarIntE_OutOfRangeReturnsError(t *testing.T) {
	e := encoding.NewEncoder()
	err := e.WriteVarIntE(1 << 56)
	require.ErrorIs(t, err, encoding.ErrVarIntOutOfRange)
	assert.Empty(t, e.Bytes(), "no bytes should be written on error")
}

func TestEncoder_WriteVarInt_StillPanicsOnOutOfRange(t *testing.T) {
	assert.Panics(t, func() {
		e := encoding.NewEncoder()
		e.WriteVarInt(1 << 56)
	})
}

// --- #77: lib0 Any tagged-union encoding parity ---

// G1: ints outside lib0's int32 range must NOT use tag 125. lib0 emits tag
// 123 (float64) for ints whose magnitude exceeds int32 range; ygo previously
// always used tag 125, breaking byte-equality with Yjs-JS-produced fixtures.
func TestUnit_Any_LargeInt_UsesFloat64Tag_NotInt(t *testing.T) {
	// 2^35 is well outside int32 range, but inside float64 safe-int range
	// (< 2^53), so it round-trips losslessly as float64.
	val := int64(1) << 35
	e := encoding.NewEncoder()
	e.WriteAny(val)
	got := e.Bytes()
	require.NotEmpty(t, got)
	assert.Equal(t, byte(123), got[0],
		"int outside int32 range must use tag 123 (float64), matching lib0")
}

// G1 cont.: ints WITHIN int32 range still use tag 125 (preserved behavior).
func TestUnit_Any_Int32RangeInt_StillUsesIntTag(t *testing.T) {
	for _, v := range []int64{0, 1, -1, 0x7FFFFFFF, -0x80000000} {
		e := encoding.NewEncoder()
		e.WriteAny(v)
		got := e.Bytes()
		require.NotEmpty(t, got)
		assert.Equal(t, byte(125), got[0],
			"v=%d must still use tag 125 (int32 range)", v)
	}
}

// G1 cont.: ints outside float64's safe-int range (>2^53) MUST use BigInt
// (tag 122) so Go's full int64 range round-trips losslessly. lib0 JS loses
// precision in this range (Number is float64) — ygo can do better because
// Go has int64 natively.
func TestUnit_Any_VeryLargeInt_UsesBigIntTag(t *testing.T) {
	val := int64(1) << 55 // outside float64 safe-int range (2^53)
	e := encoding.NewEncoder()
	e.WriteAny(val)
	got := e.Bytes()
	require.NotEmpty(t, got)
	assert.Equal(t, byte(122), got[0],
		"int outside float64 safe-int range must use tag 122 (BigInt) to preserve precision")
}

// G2: float64 values that round-trip losslessly through float32 must be
// emitted as tag 124 (4 bytes), matching lib0's isFloat32-based narrowing.
func TestUnit_Any_LosslessFloat64_NarrowsToFloat32Tag(t *testing.T) {
	val := float64(1.5) // exact in float32
	e := encoding.NewEncoder()
	e.WriteAny(val)
	got := e.Bytes()
	require.Len(t, got, 5, "lossless float32 value: tag(1) + float32(4) = 5 bytes")
	assert.Equal(t, byte(124), got[0],
		"lossless float64 must narrow to tag 124 (float32), matching lib0")
}

// G2 cont.: float64 values that DON'T round-trip through float32 must keep
// tag 123 (8 bytes). math.Pi has more precision than float32 supports.
func TestUnit_Any_LossyFloat64_KeepsFloat64Tag(t *testing.T) {
	e := encoding.NewEncoder()
	e.WriteAny(math.Pi)
	got := e.Bytes()
	require.Len(t, got, 9, "full-precision float64: tag(1) + float64(8) = 9 bytes")
	assert.Equal(t, byte(123), got[0],
		"value not representable in float32 must stay tag 123 (float64)")
}

// G3: ReadVarString must reject invalid UTF-8 sequences. lib0 uses
// TextDecoder('utf-8', { fatal: true }) which throws on malformed input;
// ygo previously did `string(b)` which accepts any byte sequence.
func TestUnit_ReadVarString_RejectsInvalidUTF8(t *testing.T) {
	// Build a payload: VarUint(3) + 3 bytes of invalid UTF-8.
	enc := encoding.NewEncoder()
	enc.WriteVarUint(3)
	enc.WriteRaw([]byte{0xff, 0xfe, 0xfd}) // not valid UTF-8

	dec := encoding.NewDecoder(enc.Bytes())
	_, err := dec.ReadVarString()
	require.Error(t, err, "lib0's fatal:true decoder rejects malformed UTF-8")
	assert.ErrorIs(t, err, encoding.ErrInvalidUTF8)
}

// G3 cont.: valid multi-byte UTF-8 still round-trips.
func TestUnit_ReadVarString_AcceptsValidUTF8(t *testing.T) {
	enc := encoding.NewEncoder()
	enc.WriteVarString("héllo 日本語 🦄")

	dec := encoding.NewDecoder(enc.Bytes())
	got, err := dec.ReadVarString()
	require.NoError(t, err)
	assert.Equal(t, "héllo 日本語 🦄", got)
}

// --- #77 boundary cases ---

// Int32 boundary: -2^31 and 2^31-1 are still tag 125 (inside int32 range);
// 2^31 and -2^31-1 are just outside and must use tag 123 (float64).
func TestUnit_Any_IntDispatch_Int32Boundary(t *testing.T) {
	cases := []struct {
		v       int64
		wantTag byte
		desc    string
	}{
		{v: 0x7FFFFFFF, wantTag: 125, desc: "max int32 (2^31-1): tag 125"},
		{v: -0x80000000, wantTag: 125, desc: "min int32 (-2^31): tag 125"},
		{v: 0x80000000, wantTag: 123, desc: "max int32 + 1 (2^31): tag 123"},
		{v: -0x80000001, wantTag: 123, desc: "min int32 - 1 (-2^31 - 1): tag 123"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			e := encoding.NewEncoder()
			e.WriteAny(tc.v)
			require.NotEmpty(t, e.Bytes())
			assert.Equal(t, tc.wantTag, e.Bytes()[0])
		})
	}
}

// Safe-int boundary: 2^53 and -2^53 are exactly representable in float64
// and use tag 123. Values just outside (2^53+1, -2^53-1) lose precision in
// float64 and must use tag 122 (BigInt) for lossless round-trip.
func TestUnit_Any_IntDispatch_SafeIntBoundary(t *testing.T) {
	cases := []struct {
		v       int64
		wantTag byte
		desc    string
	}{
		{v: 1 << 53, wantTag: 123, desc: "2^53 (max safe-int): tag 123"},
		{v: -(1 << 53), wantTag: 123, desc: "-2^53 (min safe-int): tag 123"},
		{v: (1 << 53) + 1, wantTag: 122, desc: "2^53 + 1 (precision-lossy): tag 122 BigInt"},
		{v: -(1 << 53) - 1, wantTag: 122, desc: "-2^53 - 1: tag 122 BigInt"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			e := encoding.NewEncoder()
			e.WriteAny(tc.v)
			require.NotEmpty(t, e.Bytes())
			assert.Equal(t, tc.wantTag, e.Bytes()[0])
		})
	}
}

// uint64 values above math.MaxInt64 can't be expressed as int64 (which BigInt
// wraps) and fall back to tag 123 (float64) with documented precision loss.
// Test pins that it doesn't panic and produces the expected wire shape.
func TestUnit_Any_VeryLargeUint64_FallsBackToFloat64(t *testing.T) {
	v := uint64(math.MaxUint64)
	e := encoding.NewEncoder()
	assert.NotPanics(t, func() { e.WriteAny(v) })
	got := e.Bytes()
	require.Len(t, got, 9, "tag(1) + float64(8) = 9 bytes")
	assert.Equal(t, byte(123), got[0], "uint64 > MaxInt64 must use tag 123 (float64)")
	// Round-trip returns float64 with precision loss (expected — same as JS).
	dec := encoding.NewDecoder(got)
	decoded, err := dec.ReadAny()
	require.NoError(t, err)
	assert.IsType(t, float64(0), decoded)
}

// IEEE-754 specials: NaN can't equal itself so isFloat32Lossless returns false
// → tag 123. +Inf and -Inf round-trip through float32 exactly (Inf == Inf
// holds) → tag 124 narrowing.
func TestUnit_Any_FloatSpecials(t *testing.T) {
	t.Run("NaN_stays_float64", func(t *testing.T) {
		e := encoding.NewEncoder()
		e.WriteAny(math.NaN())
		got := e.Bytes()
		require.Len(t, got, 9)
		assert.Equal(t, byte(123), got[0],
			"NaN must use tag 123 (float64) — NaN != NaN breaks the lossless check")
		// Verify decode returns NaN (can't use Equal because NaN != NaN).
		dec := encoding.NewDecoder(got)
		decoded, err := dec.ReadAny()
		require.NoError(t, err)
		f, ok := decoded.(float64)
		require.True(t, ok)
		assert.True(t, math.IsNaN(f), "round-trip preserves NaN-ness")
	})
	t.Run("PosInf_narrows_to_float32", func(t *testing.T) {
		e := encoding.NewEncoder()
		e.WriteAny(math.Inf(1))
		got := e.Bytes()
		require.Len(t, got, 5)
		assert.Equal(t, byte(124), got[0],
			"+Inf is exactly representable in float32 — must narrow to tag 124")
		dec := encoding.NewDecoder(got)
		decoded, err := dec.ReadAny()
		require.NoError(t, err)
		f, ok := decoded.(float32)
		require.True(t, ok)
		assert.True(t, math.IsInf(float64(f), 1))
	})
	t.Run("NegInf_narrows_to_float32", func(t *testing.T) {
		e := encoding.NewEncoder()
		e.WriteAny(math.Inf(-1))
		got := e.Bytes()
		require.Len(t, got, 5)
		assert.Equal(t, byte(124), got[0])
	})
}

// Asymmetric UTF-8 contract: WriteVarString trusts the caller (Go strings
// can legally contain invalid UTF-8 byte sequences). ReadVarString is the
// validation gate. Document this so future contributors don't add
// validation to WriteVarString and break callers passing pre-encoded data.
func TestUnit_VarString_AsymmetricUTF8Contract(t *testing.T) {
	invalid := string([]byte{0xff, 0xfe, 0xfd}) // not valid UTF-8

	// Write trusts the caller — no validation, no panic.
	enc := encoding.NewEncoder()
	assert.NotPanics(t, func() { enc.WriteVarString(invalid) })

	// Read validates and rejects.
	dec := encoding.NewDecoder(enc.Bytes())
	_, err := dec.ReadVarString()
	require.Error(t, err)
	assert.ErrorIs(t, err, encoding.ErrInvalidUTF8,
		"asymmetric contract: write trusts, read validates")
}

// G4: WriteAny must accept Go unsigned integer types (uint, uint8-32, uint64
// within int64 range) by promoting to int64. lib0 has no native unsigned
// type; the goal is API ergonomic parity with Go's numeric tower.
func TestUnit_Any_AcceptsUnsignedInts(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want int64
	}{
		{"uint", uint(42), 42},
		{"uint8", uint8(42), 42},
		{"uint16", uint16(42), 42},
		{"uint32", uint32(42), 42},
		{"uint64_small", uint64(42), 42},
		{"int8", int8(-42), -42},
		{"int16", int16(-42), -42},
		{"int32", int32(-42), -42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := encoding.NewEncoder()
			assert.NotPanics(t, func() { e.WriteAny(tc.val) })
			got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
			require.NoError(t, err)
			// All small values land in int32 range -> tag 125 -> ReadAny returns int64.
			assert.Equal(t, tc.want, got)
		})
	}
}
