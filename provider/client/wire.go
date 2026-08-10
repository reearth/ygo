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
