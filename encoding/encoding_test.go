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
		// VarInt is bounded by the 53-bit VarUint limit.
		// ZigZag maps positive n to 2n, so max is (2^53-1)/2 = 2^52-1.
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
	// -1 should encode to 2 bytes or fewer (ZigZag maps it to 1, which is 1 byte).
	e := encoding.NewEncoder()
	e.WriteVarInt(-1)
	assert.Len(t, e.Bytes(), 1, "-1 should ZigZag-encode to a single byte")
}

// --- VarString ---

func TestUnit_VarString_RoundTrip(t *testing.T) {
	cases := []string{
		"",
		"hello",
		"こんにちは",    // multibyte UTF-8
		"😀🎉🚀",     // 4-byte codepoints
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
	// WriteAny accepts plain int; ReadAny always returns int64.
	e := encoding.NewEncoder()
	e.WriteAny(int(42))
	got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
	require.NoError(t, err)
	assert.Equal(t, int64(42), got)
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
