// Package websocket - internal test for encodeAuthMessage's UTF-8 coercion
// (#209). This lives in the internal (package websocket) test set because
// encodeAuthMessage is unexported and not reachable from package
// websocket_test.
package websocket

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// TestUnit_EncodeAuthMessage_CoercesInvalidUTF8 guards issue #209.
// encodeAuthMessage's s argument is app-supplied (an OnTokenAuth hook's
// error text, or the "read-write"/"readonly" scope label) and runs on a
// live connection goroutine. Task 1 made WriteVarString panic on invalid
// UTF-8, so encodeAuthMessage must coerce before encoding rather than let a
// malformed app error string panic the goroutine and drop the peer.
//
// The contract under test is the varstring payload, checked by decoding it
// back. Do not assert utf8.Valid over the whole framed message: the length
// varuint is not UTF-8 and legitimately carries continuation-range bytes
// once the payload reaches 128 bytes (0x80 0x01), so such an assertion
// fails on a perfectly decodable message. The long case below is that shape.
func TestUnit_EncodeAuthMessage_CoercesInvalidUTF8(t *testing.T) {
	cases := []struct {
		name string
		s    string
	}{
		// One-byte length varuint.
		{"short", "denied " + string([]byte{0xff})},
		// A realistic long app error string, forcing a multi-byte length
		// varuint in the framing.
		{"long multi-byte length varuint", "denied " + strings.Repeat("x", 200) + string([]byte{0xff})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An app hook returning a malformed error string must not panic
			// the connection goroutine.
			var out []byte
			require.NotPanics(t, func() {
				out = encodeAuthMessage(authTypePermissionDenied, tc.s)
			})
			require.NotEmpty(t, out)

			// The payload must round-trip through the decoder, which rejects
			// invalid UTF-8 — that is the coercion's actual guarantee.
			dec := encoding.NewDecoder(out)
			msgType, err := dec.ReadVarUint()
			require.NoError(t, err)
			require.Equal(t, msgAuth, msgType)
			subType, err := dec.ReadVarUint()
			require.NoError(t, err)
			require.Equal(t, authTypePermissionDenied, subType)
			s, err := dec.ReadVarString()
			require.NoError(t, err, "coerced payload must itself be valid UTF-8")
			require.Contains(t, s, "denied ")
			require.NotContains(t, s, string([]byte{0xff}))
		})
	}
}
