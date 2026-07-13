package encoding_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// TestUnit_StringEncoder_RoundTripSequential guards StringDecoder.Read's
// incremental UTF-16 -> byte offset tracking (the O(n^2) -> O(n) perf fix).
// Sequential reads across ASCII, multi-byte, surrogate-pair (each 2 UTF-16
// units), and empty strings must slice the shared column string at the exact
// boundaries, not drift.
func TestUnit_StringEncoder_RoundTripSequential(t *testing.T) {
	inputs := []string{
		"hello",
		"",     // empty at the start of a read
		"café", // 2-byte codepoint
		"日本語",  // 3-byte codepoints
		"😀🎉",   // surrogate pairs
		"a😀b",  // ASCII mixed with a surrogate pair
		"",     // empty mid-stream
		"tail",
	}

	var enc encoding.StringEncoder
	for _, s := range inputs {
		enc.Write(s)
	}

	dec, err := encoding.NewStringDecoder(enc.Bytes())
	require.NoError(t, err)

	for i, want := range inputs {
		got, err := dec.Read()
		require.NoError(t, err, "read %d", i)
		require.Equal(t, want, got, "string %d", i)
	}
}
