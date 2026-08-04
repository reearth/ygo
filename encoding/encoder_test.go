package encoding

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #209 — Encoder.WriteVarString appended a Go string's bytes without
// validating them, while ReadVarString rejects invalid UTF-8. That let a
// caller encode an update that no decoder (ygo's or Yjs's) could read.
// These tests pin the encoder-level net: WriteVarStringE returns
// ErrInvalidUTF8 on invalid input, and WriteVarString panics.

func TestUnit_Encoder_WriteVarStringE_RejectsInvalidUTF8(t *testing.T) {
	e := NewEncoder()
	err := e.WriteVarStringE(string([]byte{0xff, 0xfe}))
	require.ErrorIs(t, err, ErrInvalidUTF8)
	require.Empty(t, e.Bytes(), "nothing may be written when validation fails")
}

func TestUnit_Encoder_WriteVarStringE_EncodedSurrogateIsInvalid(t *testing.T) {
	// ED A0 80 is U+D800 encoded as UTF-8: the class JS produces. lib0's
	// TextDecoder rejects it, so we must too.
	e := NewEncoder()
	require.ErrorIs(t, e.WriteVarStringE(string([]byte{0xED, 0xA0, 0x80})), ErrInvalidUTF8)
}

func TestUnit_Encoder_WriteVarString_PanicsOnInvalidUTF8(t *testing.T) {
	e := NewEncoder()
	require.PanicsWithValue(t,
		"encoding: WriteVarString: input is not valid UTF-8",
		func() { e.WriteVarString(string([]byte{0xff})) })
}

func TestUnit_Encoder_WriteVarStringE_ValidIsByteIdentical(t *testing.T) {
	for _, s := range []string{"", "hello", "héllo wörld", "😀 emoji", "á combining"} {
		want := NewEncoder()
		want.WriteVarUint(uint64(len(s)))
		want.WriteRaw([]byte(s))

		got := NewEncoder()
		require.NoError(t, got.WriteVarStringE(s))
		require.Equal(t, want.Bytes(), got.Bytes(), "input %q", s)
	}
}
