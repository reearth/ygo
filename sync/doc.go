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
