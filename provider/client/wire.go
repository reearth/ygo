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
