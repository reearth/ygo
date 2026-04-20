// Server-side document injection — types, error sentinels, and hook signature.
// BroadcastUpdate, Apply, and CloseRoom are defined in this file; their
// bodies are populated in later tasks.
package websocket

import (
	"context"
	"errors"
	"fmt"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
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

// effectiveMaxUpdateBytes returns the server's configured per-update
// cap, or the default 64 MiB (matching the peer frame cap) when unset.
func (s *Server) effectiveMaxUpdateBytes() int {
	if s.MaxUpdateBytes > 0 {
		return s.MaxUpdateBytes
	}
	return int(maxWSMessageBytes)
}

// BroadcastUpdate fans out a pre-encoded V1 update to all peers
// currently connected to the named room. It does NOT apply the update
// to the server's doc; callers who want the server's state to reflect
// the broadcast must call crdt.ApplyUpdateV1 first (or use Apply).
// Failing to do so creates divergence: live peers see the update, but
// peers joining after the broadcast receive the server's stale state
// via sync step 2.
//
// Peer write failures during fan-out do not produce an error: writes
// are dispatched in goroutines with a per-write deadline (writeTimeout),
// matching the existing peer-broadcast path. A slow peer cannot block
// the broadcast to other peers.
func (s *Server) BroadcastUpdate(ctx context.Context, room string, update []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.shutdownCh:
		return ErrServerShutdown
	default:
	}
	if !isValidRoomName(room) {
		return ErrInvalidRoomName
	}
	if len(update) > s.effectiveMaxUpdateBytes() {
		return ErrUpdateTooLarge
	}
	// Validate by applying to a throwaway doc. If the bytes are
	// malformed, peers would reject them anyway; catching at the
	// server boundary surfaces caller bugs eagerly.
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidUpdate, err)
	}
	if s.OnInject != nil {
		if err := s.OnInject(ctx, InjectInfo{
			Room:       room,
			Op:         OpBroadcastUpdate,
			UpdateSize: len(update),
		}); err != nil {
			return fmt.Errorf("ygo/websocket: inject refused: %w", err)
		}
	}
	s.rmu.RLock()
	rm, ok := s.rooms[room]
	s.rmu.RUnlock()
	if !ok {
		return ErrRoomNotFound
	}
	rm.mu.Lock()
	targets := make([]*peer, 0, len(rm.peers))
	for p := range rm.peers {
		targets = append(targets, p)
	}
	rm.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	data := encodeBroadcastWire(update)
	for _, p := range targets {
		go p.write(data)
	}
	return nil
}

// encodeBroadcastWire wraps a V1 update in the outer sync frame used by
// both peer and server-side broadcasts:
//
//	[msgSync][MsgUpdate][VarBytes(update bytes)]
//
// The outer sync type byte is NOT VarBytes-wrapped (matching broadcastSync),
// but the update payload inside the sync message IS VarBytes-wrapped (matching
// what ApplySyncMessage expects for MsgUpdate and MsgSyncStep2 messages).
func encodeBroadcastWire(update []byte) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(msgSync)
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	return enc.Bytes()
}
