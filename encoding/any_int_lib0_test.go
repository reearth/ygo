package encoding_test

import (
	"encoding/hex"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// A Go integer is written as lib0 0.2.119's writeAny writes the same value as
// a JavaScript Number: tag 125 while |v| <= 2^31-1, then tag 124 (float32)
// when a float32 holds the value exactly, then tag 123 (float64). The bytes
// below are lib0's own, from writeAny in Node, so the test pins the wire form
// and not only the tag. A value lib0 writes as tag 124 or 123 reads back as a
// float64, as lib0's readAny reads either as a Number.
func TestUnit_Any_IntegerOutsideInt32_IsWrittenAsLib0Writes(t *testing.T) {
	cases := []struct {
		name string
		v    any
		lib0 string // lib0 0.2.119 writeAny(encoder, Number(v)), hex
	}{
		{"2^31-1", int64(math.MaxInt32), "7dbfffffff0f"},
		{"-(2^31-1)", int64(-math.MaxInt32), "7dffffffff0f"},
		{"2^31", int64(1 << 31), "7c4f000000"},
		{"-2^31", int64(math.MinInt32), "7ccf000000"},
		{"2^31+1", int64(1<<31 + 1), "7b41e0000000200000"},
		{"-2^31-1", int64(-(1 << 31) - 1), "7bc1e0000000200000"},
		{"2^40", int64(1 << 40), "7c53800000"},
		{"-2^40", int64(-(1 << 40)), "7cd3800000"},
		{"3*2^35", int64(3 << 35), "7c51c00000"},
		{"2^53", int64(1 << 53), "7c5a000000"},
		{"-2^53", int64(-(1 << 53)), "7cda000000"},
		{"2^53-1", int64(1<<53 - 1), "7b433fffffffffffff"},
		{"uint64 2^63", uint64(1 << 63), "7c5f000000"},
		{"uint64 max", uint64(math.MaxUint64), "7c5f800000"},
		{"int -2^31", int(math.MinInt32), "7ccf000000"}, // a 32-bit int holds it too
		{"uint32 2^31", uint32(1 << 31), "7c4f000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := hex.DecodeString(tc.lib0)
			require.NoError(t, err)
			e := encoding.NewEncoder()
			e.WriteAny(tc.v)
			assert.Equal(t, tc.lib0, hex.EncodeToString(e.Bytes()), "the bytes lib0 writes")
			assert.Len(t, e.Bytes(), len(want))

			got, err := encoding.NewDecoder(e.Bytes()).ReadAny()
			require.NoError(t, err)
			switch want[0] {
			case 125:
				assert.Equal(t, tc.v, got, "tag 125 reads back as an int64")
			case 124, 123:
				f, ok := got.(float64)
				require.True(t, ok, "tag %d reads back as a float64; got %T", want[0], got)
				// Delta 0: a float32 holds every tag-124 value of this class exactly.
				assert.InDelta(t, asFloat64(tc.v), f, 0)
			}
		})
	}
}

// An int64 past float64's safe-integer range keeps tag 122 (BigInt), the form
// lib0 writes for a JavaScript BigInt, even when a float32 holds it: a Number
// could not carry the neighbouring values exactly, and ygo keeps the integer.
func TestUnit_Any_IntegerPastSafeRange_StaysBigInt(t *testing.T) {
	for _, v := range []int64{1 << 60, -(1 << 60), 1<<53 + 1} {
		e := encoding.NewEncoder()
		e.WriteAny(v)
		require.Len(t, e.Bytes(), 9)
		assert.Equal(t, byte(122), e.Bytes()[0], "%d", v)
	}
}

func asFloat64(v any) float64 {
	switch v := v.(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint32:
		return float64(v)
	case uint64:
		return float64(v)
	}
	panic("unexpected type")
}
