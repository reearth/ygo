package encoding

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/internal/anycodec"
)

func TestUnit_SkipAny_ConsumesAndValidatesLikeReadAny(t *testing.T) {
	values := []any{nil, true, false, int64(123456), float32(1.5), float64(1.25), BigInt(55), "Ключ 🐷", []byte{1, 2}, []any{1, "s", map[string]any{"nested": []any{true, nil}}}}
	for _, value := range values {
		enc := NewEncoder()
		enc.WriteAny(value)
		enc.WriteUint8(42)
		data := enc.Bytes()
		for length := 0; length <= len(data); length++ {
			a := NewDecoder(data[:length])
			_, want := a.ReadAny()
			consumed, err := anycodec.Skip(data[:length])
			require.Equal(t, want, err)
			require.Equal(t, a.Remaining(), length-consumed)
		}
		var err error
		allocs := testing.AllocsPerRun(1, func() { _, err = anycodec.Skip(data) })
		require.NoError(t, err)
		require.Zero(t, allocs, "skip must not materialize nested values")
	}
	for _, data := range [][]byte{
		{0},                    // unknown tag
		{119, 1, 0xff},         // invalid UTF-8 value
		{118, 1, 1, 0xff, 126}, // invalid UTF-8 map key
	} {
		_, want := NewDecoder(data).ReadAny()
		require.Error(t, want)
		_, err := anycodec.Skip(data)
		require.Equal(t, want, err)
	}
	enc := NewEncoder()
	for i := 0; i < maxAnyDepth+2; i++ {
		enc.WriteUint8(117)
		enc.WriteVarUint(1)
	}
	enc.WriteUint8(126)
	_, err := anycodec.Skip(enc.Bytes())
	require.ErrorIs(t, err, ErrDepthExceeded)
	enc = NewEncoder()
	enc.WriteUint8(117)
	enc.WriteVarUint(maxAnyElements + 1)
	_, err = anycodec.Skip(enc.Bytes())
	require.ErrorIs(t, err, ErrDepthExceeded)
}
