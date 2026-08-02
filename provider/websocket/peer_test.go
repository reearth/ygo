// Package websocket - internal test for encodeAuthMessage's UTF-8 coercion
// (#209). This lives in the internal (package websocket) test set because
// encodeAuthMessage is unexported and not reachable from package
// websocket_test.
package websocket

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// TestUnit_EncodeAuthMessage_CoercesInvalidUTF8 guards issue #209.
// encodeAuthMessage's s argument is app-supplied (an OnTokenAuth hook's
// error text, or the "read-write"/"readonly" scope label) and runs on a
// live connection goroutine. Task 1 made WriteVarString panic on invalid
// UTF-8, so encodeAuthMessage must coerce before encoding rather than let a
// malformed app error string panic the goroutine and drop the peer.
func TestUnit_EncodeAuthMessage_CoercesInvalidUTF8(t *testing.T) {
	// An app hook returning a malformed error string must not panic the
	// connection goroutine.
	var out []byte
	require.NotPanics(t, func() {
		out = encodeAuthMessage(authTypePermissionDenied, "denied "+string([]byte{0xff}))
	})
	require.True(t, utf8.Valid(out), "framed message must be decodable")

	// The payload itself must also round-trip through the decoder: prove
	// this isn't just an accident of the varuint framing bytes happening to
	// be valid UTF-8 on their own.
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
}
