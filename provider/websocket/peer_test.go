// Package websocket - internal tests for encodeAuthMessage's UTF-8 coercion
// (#209) and encodeAwarenessRemoval's clock behavior (#226). Both live in the
// internal (package websocket) test set because the functions under test are
// unexported and not reachable from package websocket_test.
package websocket

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
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

// TestUnit_EncodeAwarenessRemoval_UnbumpedSurvivesRejoinTie guards #226.
//
// encodeAwarenessRemoval used to synthesise a removal at the room's current
// clock for that client PLUS ONE. Awareness.Heartbeat, called by a rejoining
// client re-announcing itself, ALSO bumps by exactly one from the same base
// clock (the client's own last-known clock, which — absent any intervening
// update — is identical to what the room last saw). So a disconnect and a
// same-client rejoin computed from that shared base landed on the SAME
// clock. At an equal clock, Awareness.ApplyUpdate's tie-break rule always
// favors the null (removal) side over an active state, regardless of which
// one a given peer happens to receive first — see the two subtests below,
// which apply the pair in both orders. A third, uninvolved peer receiving
// both therefore always ended up with the rejoining client marked removed:
// a genuine reappearance silently suppressed.
//
// The fix leaves the removal clock unbumped, matching the room's live view
// exactly. That makes the rejoin heartbeat's own bump strictly newer than
// the removal in every case, so the tie — and the suppression it caused —
// no longer arises, regardless of arrival order.
func TestUnit_EncodeAwarenessRemoval_UnbumpedSurvivesRejoinTie(t *testing.T) {
	const rejoiningClient = uint64(42)

	// buildScenario re-creates, from scratch each time, a disconnect and a
	// same-client rejoin computed from the identical base clock: the room's
	// shared Awareness and the rejoining client's own Awareness both start
	// out having just seen the client announce itself once (clock 1).
	buildScenario := func(t *testing.T) (removal, rejoinHeartbeat []byte, peerC *awareness.Awareness) {
		t.Helper()

		client := awareness.New(rejoiningClient)
		client.SetLocalState(map[string]any{"cursor": float64(1)})
		announce := client.EncodeUpdate(nil)

		// The room's shared Awareness (server-side) and a third, uninvolved
		// peer both learn of the client's announcement at clock 1.
		room := awareness.New(999)
		require.NoError(t, room.ApplyUpdate(announce, nil))

		peerC = awareness.New(3)
		require.NoError(t, peerC.ApplyUpdate(announce, nil))

		// Disconnect: the server synthesises a removal from the room's
		// CURRENT view of the client — clock 1, same base as below.
		removal = encodeAwarenessRemoval(room, []uint64{rejoiningClient})
		require.NotNil(t, removal)

		// Rejoin: the SAME client heartbeats from that SAME base clock (1).
		client.Heartbeat()
		rejoinHeartbeat = client.EncodeUpdate(nil)

		return removal, rejoinHeartbeat, peerC
	}

	t.Run("removal arrives before rejoin heartbeat", func(t *testing.T) {
		removal, rejoin, peerC := buildScenario(t)
		require.NoError(t, peerC.ApplyUpdate(removal, nil))
		require.NoError(t, peerC.ApplyUpdate(rejoin, nil))

		_, present := peerC.GetStates()[rejoiningClient]
		require.True(t, present, "rejoining client's presence must survive the tie")
	})

	t.Run("rejoin heartbeat arrives before removal", func(t *testing.T) {
		removal, rejoin, peerC := buildScenario(t)
		require.NoError(t, peerC.ApplyUpdate(rejoin, nil))
		require.NoError(t, peerC.ApplyUpdate(removal, nil))

		_, present := peerC.GetStates()[rejoiningClient]
		require.True(t, present, "rejoining client's presence must survive the tie")
	})
}
