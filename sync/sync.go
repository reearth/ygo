// Package sync implements the y-protocols binary sync protocol.
//
// The protocol is transport-agnostic: SyncStep1, SyncStep2, and Update
// messages are plain []byte that can be sent over WebSocket, HTTP,
// WebRTC, or in-process pipes.
//
// Typical two-peer handshake:
//
//	// Peer A sends its state vector
//	step1 := sync.EncodeSyncStep1(docA)
//
//	// Peer B responds with missing updates
//	step2, _ := sync.EncodeSyncStep2(docB, step1)
//
//	// Peer A applies the response
//	sync.ApplySyncMessage(docA, step2)
//
// Reference: https://github.com/yjs/y-protocols/blob/master/PROTOCOL.md
package sync

import (
	"errors"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
)

// Message type constants as defined by y-protocols.
const (
	MsgSyncStep1 = 0
	MsgSyncStep2 = 1
	MsgUpdate    = 2
)

var (
	ErrUnexpectedEOF    = errors.New("sync: unexpected end of message")
	ErrUnknownMessage   = errors.New("sync: unknown message type")
)

// EncodeSyncStep1 encodes a sync-step-1 message containing doc's state vector.
// The receiver should respond with EncodeSyncStep2.
func EncodeSyncStep1(doc *crdt.Doc) []byte {
	sv := crdt.EncodeStateVectorV1(doc)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(MsgSyncStep1)
	enc.WriteVarBytes(sv)
	return enc.Bytes()
}

// EncodeSyncStep2 decodes the state vector from a step-1 message and returns
// a sync-step-2 message containing the updates the remote peer is missing.
func EncodeSyncStep2(doc *crdt.Doc, step1msg []byte) ([]byte, error) {
	dec := encoding.NewDecoder(step1msg)

	msgType, err := dec.ReadVarUint()
	if err != nil {
		return nil, ErrUnexpectedEOF
	}
	if msgType != MsgSyncStep1 {
		return nil, ErrUnknownMessage
	}

	svBytes, err := dec.ReadVarBytes()
	if err != nil {
		return nil, ErrUnexpectedEOF
	}

	sv, err := crdt.DecodeStateVectorV1(svBytes)
	if err != nil {
		return nil, err
	}

	update := crdt.EncodeStateAsUpdateV1(doc, sv)

	enc := encoding.NewEncoder()
	enc.WriteVarUint(MsgSyncStep2)
	enc.WriteVarBytes(update)
	return enc.Bytes(), nil
}

// EncodeUpdate wraps a raw V1 update in a sync update message (type 2).
// Use this to broadcast incremental updates to peers after a local change.
func EncodeUpdate(update []byte) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(MsgUpdate)
	enc.WriteVarBytes(update)
	return enc.Bytes()
}

// ApplySyncMessage decodes a sync message and applies it to doc.
// It handles all three message types:
//   - step-1: returns a step-2 reply that should be sent back to the sender
//   - step-2: applies the enclosed update; reply is nil
//   - update:  applies the enclosed update; reply is nil
//
// The origin value is passed through to doc.ApplyUpdate and can be used
// by observers to identify the source of an update (e.g. a connection ID).
func ApplySyncMessage(doc *crdt.Doc, msg []byte, origin any) (reply []byte, err error) {
	dec := encoding.NewDecoder(msg)

	msgType, err := dec.ReadVarUint()
	if err != nil {
		return nil, ErrUnexpectedEOF
	}

	switch msgType {
	case MsgSyncStep1:
		// Re-encode the full message so EncodeSyncStep2 can re-read the type byte.
		return EncodeSyncStep2(doc, msg)

	case MsgSyncStep2, MsgUpdate:
		updateBytes, err := dec.ReadVarBytes()
		if err != nil {
			return nil, ErrUnexpectedEOF
		}
		if err := crdt.ApplyUpdateV1(doc, updateBytes, origin); err != nil {
			return nil, err
		}
		return nil, nil

	default:
		return nil, ErrUnknownMessage
	}
}
