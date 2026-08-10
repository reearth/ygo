package client

import "github.com/reearth/ygo/encoding"

// Outer y-websocket envelope message types (#165). These mirror
// provider/websocket/server.go's msgSync/msgAwareness/msgAuth/
// msgQueryAwareness byte-for-byte — the client and ygo's own server (and,
// transitively, y-websocket/y-protocols) must agree on these tags to
// interoperate on the same connection. Only the first four y-protocols core
// tags are needed here: the client drives sync and awareness, it does not
// speak the Hocuspocus extensions (tags 4+) the server separately accepts.
const (
	wireMsgSync           = uint64(0)
	wireMsgAwareness      = uint64(1)
	wireMsgAuth           = uint64(2)
	wireMsgQueryAwareness = uint64(3)
)

// encodeEnvelope builds a y-websocket outer frame: VarUint(msgType) followed
// by payload appended raw, with no length prefix or other structure imposed
// on it. This matches provider/websocket/peer.go's sendSync/sendAwareness,
// which both write the outer tag via WriteVarUint and then append their
// payload verbatim (WriteRaw for sync, WriteVarBytes for awareness) — the
// VarBytes-wrapping for awareness, when needed, is the caller's concern:
// payload for that case is the already-length-prefixed bytes, and
// encodeEnvelope just appends them. This split keeps encodeEnvelope generic
// across every outer message type without hard-coding per-type payload
// shapes, matching how decodeEnvelope hands back the same opaque remainder.
func encodeEnvelope(msgType uint64, payload []byte) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgType)
		enc.WriteRaw(payload)
	})
}

// Hocuspocus in-band Auth (tag 2) sub-types (#104). These mirror
// provider/websocket/server.go's authTypeToken/authTypePermissionDenied/
// authTypeAuthenticated byte-for-byte — see that package's peer.go
// handleAuth (reads Token) and encodeAuthMessage (writes
// PermissionDenied/Authenticated), which this client's auth exchange
// (loop.go, #165 Task 9) must match exactly to interoperate with ygo's own
// server. Pinned against the server's values in auth_test.go.
const (
	// authTypeToken is the client->server sub-message this client sends:
	// VarUint(authTypeToken) VarString(token).
	authTypeToken = uint64(0)
	// authTypePermissionDenied is a server->client reply: the token was
	// rejected (a non-nil error from Server.OnTokenAuth). Carries the
	// hook's error text as its accompanying string.
	authTypePermissionDenied = uint64(1)
	// authTypeAuthenticated is a server->client reply: the token was
	// accepted. Carries the granted scope ("read-write" or "readonly") as
	// its accompanying string.
	authTypeAuthenticated = uint64(2)
)

// wsCodeUnauthorized mirrors provider/websocket/server.go's identically
// named unexported constant (4401, the WebSocket close code
// peer.handleAuth's rejection path passes to enqueueClose alongside the
// PermissionDenied data frame). Like authTypeToken and friends above, this
// is pinned by VALUE rather than by importing provider/websocket, since the
// server's constant is itself unexported.
//
// # Why this client needs its own copy of a close code (#165)
//
// Before this constant existed, a rejected token was ONLY detectable from
// the PermissionDenied data frame (handleFrame's wireMsgAuth case, below) —
// nothing else on this connection was treated as evidence of a rejection.
// That is provably insufficient on its own: peer.go's sendCloseFrame calls
// conn.Close() immediately after queuing the close control frame that
// follows PermissionDenied, and closing a TCP socket that still has *unread
// inbound* bytes buffered from this client (this client's handleFrame
// replies to the server's own unconditional initial SyncStep1/awareness
// push before it has any idea its Token is about to be rejected, so there
// is normally something of this client's own still sitting unread
// server-side at exactly this moment) is a textbook trigger for the kernel
// to emit an RST instead of a graceful FIN — and a well-established
// property of RST-terminated connections is that data already sitting in
// the OTHER side's own unread receive buffer is not guaranteed to survive
// it. A client that never gets to read the PermissionDenied frame before
// its connection dies this way sees only a close, indistinguishable from
// any other severed connection, with the ordinary read-error handling below
// (before this constant) then backing off and redialing the same doomed
// token.
//
// That specific RST mechanism is this file's best explanation for the
// intermittent CI failure that motivated this fix (auth_test.go's
// TestClient_Auth_WrongTokenIsTerminal, driving ygo's own
// provider/websocket.Server, occasionally saw a second OnTokenAuth call —
// never reproduced locally across 30 -race runs) — it is INFERRED from how
// peer.go and gorilla/websocket both behave, not something this package
// instrumented and captured directly off a real socket. What IS directly
// established, independent of that inference being exactly right, is the
// defect itself: this client's terminality check depended entirely on
// reading one specific frame, with nothing to fall back on if that frame
// were ever lost for ANY reason. auth_test.go's
// TestClient_Auth_UnauthorizedCloseWithoutDenialFrameIsTerminal proves this
// deterministically — no RST or scheduling race required — with a raw
// server that reads the client's frames normally (so nothing about THIS
// client's own sends fails) but sends only this close code, never a
// PermissionDenied frame, at all. See runLoop's readErr case for where the
// close code is checked (via gws.IsCloseError) as the fallback signal.
const wsCodeUnauthorized = 4401

// encodeAuthToken builds the client->server Auth (tag 2) frame carrying a
// Token (sub-type 0) sub-message: envelope(wireMsgAuth,
// VarUint(authTypeToken) VarString(token)). This is the exact shape
// provider/websocket/peer.go's handleAuth reads off the wire — see
// hocuspocus_auth_internal_test.go's TestInbandAuth_Authenticated_ReadWrite
// for the server-side proof of the same bytes (#104, #165 Task 9).
//
// Deliberately NOT docName-prefixed: this client only ever speaks the
// plain, non-HocuspocusFraming wire variant (see the package's design
// non-goals — "the two shipped framings only"), and provider/websocket's
// OnTokenAuth does not require HocuspocusFraming to be enabled — only
// Server.HocuspocusFraming's own docName read/write does, which is an
// orthogonal, unrelated knob.
func encodeAuthToken(token string) []byte {
	return encodeEnvelope(wireMsgAuth, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString(token)
	}))
}

// decodeAuthReply parses the payload of a server->client Auth (tag 2) frame
// — as produced by provider/websocket/peer.go's encodeAuthMessage — into its
// sub-type (authTypeAuthenticated or authTypePermissionDenied) and
// accompanying string (the granted scope, or the denial reason,
// respectively). payload is decodeEnvelope's returned remainder for a
// wireMsgAuth frame, i.e. everything after the outer VarUint tag.
func decodeAuthReply(payload []byte) (subType uint64, s string, err error) {
	dec := encoding.NewDecoder(payload)
	subType, err = dec.ReadVarUint()
	if err != nil {
		return 0, "", err
	}
	s, err = dec.ReadVarString()
	if err != nil {
		return 0, "", err
	}
	return subType, s, nil
}

// decodeEnvelope splits a y-websocket outer frame into its message type and
// payload. The payload is everything after the outer VarUint tag, returned
// as-is with no further interpretation — mirroring provider/websocket/
// peer.go's handleMessage, which reads outerType then dispatches on it
// before deciding how (or whether) to unwrap the remainder. That dispatch,
// and any inner VarBytes-unwrapping for awareness frames, is a later #165
// task's job (the sync loop that consumes this codec); decodeEnvelope's
// only contract is the outer layer.
//
// The returned payload aliases frame; callers that need an independent copy
// must copy it themselves, consistent with encoding.Decoder.RemainingBytes.
func decodeEnvelope(frame []byte) (msgType uint64, payload []byte, err error) {
	dec := encoding.NewDecoder(frame)
	msgType, err = dec.ReadVarUint()
	if err != nil {
		return 0, nil, err
	}
	return msgType, dec.RemainingBytes(), nil
}
