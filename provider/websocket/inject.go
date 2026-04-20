// Package websocket: server-side document injection.
//
// This file adds APIs that let server-side Go code (AI agents, HTTP
// handlers, content pipelines) push changes into a live room without
// simulating a WebSocket peer. See docs/superpowers/specs/ for the
// full design rationale.
package websocket

import (
	"context"
	"errors"

	"github.com/reearth/ygo/crdt"
)

// InjectOp identifies which server-side write path is being invoked.
type InjectOp int

const (
	// OpBroadcastUpdate is passed to OnInject when BroadcastUpdate is
	// the calling method.
	OpBroadcastUpdate InjectOp = iota
	// OpApply is passed to OnInject when Apply is the calling method.
	OpApply
)

// String returns a human-readable name for the op.
func (o InjectOp) String() string {
	switch o {
	case OpBroadcastUpdate:
		return "BroadcastUpdate"
	case OpApply:
		return "Apply"
	default:
		return "unknown"
	}
}

// InjectInfo is passed to OnInject. Additional fields may be added in
// future versions; callers must not rely on the struct being fixed-size.
type InjectInfo struct {
	// Room is the room name the operation targets.
	Room string
	// Op identifies the calling method.
	Op InjectOp
	// UpdateSize is the length of the update bytes for BroadcastUpdate,
	// or 0 for Apply (the delta has not yet been produced).
	UpdateSize int
}

// InjectHook is called before every server-side write. Return a non-nil
// error to refuse the operation; the error is wrapped and returned to
// the caller.
type InjectHook func(ctx context.Context, info InjectInfo) error

// Error sentinels returned by BroadcastUpdate, Apply, and CloseRoom.
// Callers should compare with errors.Is rather than ==.
var (
	// ErrServerShutdown is returned when a server-side write is attempted
	// after Server.Shutdown has been called.
	ErrServerShutdown = errors.New("ygo/websocket: server is shut down")
	// ErrInvalidRoomName is returned when a room name fails validation
	// (empty, > 255 bytes, path-unsafe, or contains control characters).
	ErrInvalidRoomName = errors.New("ygo/websocket: invalid room name")
	// ErrRoomNotFound is returned when a server-side write targets a
	// room that does not currently exist. May occur if the last peer
	// disconnected concurrently; callers broadcasting to ephemeral rooms
	// should treat this as non-fatal.
	ErrRoomNotFound = errors.New("ygo/websocket: room not found")
	// ErrRoomHasPeers is returned by CloseRoom when called with force=false
	// on a room that still has connected peers.
	ErrRoomHasPeers = errors.New("ygo/websocket: room has connected peers")
	// ErrInvalidUpdate is returned when BroadcastUpdate cannot parse the
	// caller-supplied update bytes as a V1 update.
	ErrInvalidUpdate = errors.New("ygo/websocket: invalid V1 update")
	// ErrUpdateTooLarge is returned when an update exceeds MaxUpdateBytes.
	ErrUpdateTooLarge = errors.New("ygo/websocket: update exceeds MaxUpdateBytes")
	// ErrTooManyRooms is returned when auto-creating a room would
	// exceed Server.MaxRooms.
	ErrTooManyRooms = errors.New("ygo/websocket: MaxRooms exceeded")
	// ErrNoChanges is returned by Apply when fn produces no delta
	// (either never called transact or called transact with a no-op body).
	ErrNoChanges = errors.New("ygo/websocket: no changes produced")
)

// Suppress unused-import lint until later tasks add the function bodies
// that use context and crdt.
var _ context.Context
var _ = crdt.New
