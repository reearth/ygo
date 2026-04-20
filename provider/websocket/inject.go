// Server-side document injection — BroadcastUpdate, Apply, and CloseRoom
// plus their types, error sentinels, and hook signature. See doc.go for the
// package-level overview.
package websocket

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
	// ErrInjectRefused is returned when the OnInject hook returns a
	// non-nil error. The hook's error is wrapped as the cause and
	// remains reachable via errors.Unwrap.
	ErrInjectRefused = errors.New("ygo/websocket: inject refused")
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
		return fmt.Errorf("%w: %w", ErrInvalidUpdate, err)
	}
	if s.OnInject != nil {
		if err := s.OnInject(ctx, InjectInfo{
			Room:       room,
			Op:         OpBroadcastUpdate,
			UpdateSize: len(update),
		}); err != nil {
			return fmt.Errorf("%w: %w", ErrInjectRefused, err)
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

// Apply auto-creates the room if needed, runs fn with a bound transact
// helper, captures the update(s) produced by fn's transaction(s), and
// fans the result out to all connected peers.
//
// fn MUST call transact() to mutate the doc. Calls to doc.GetText,
// doc.GetMap, doc.GetXmlFragment, etc. must happen OUTSIDE transact():
// these acquire the doc's write lock, which transact() already holds,
// so calling them inside would deadlock.
//
// fn should be fast — it runs inside the doc's write lock and blocks
// all peer reads and writes to the room for the duration.
//
// IMPORTANT: if fn calls doc.Transact directly (bypassing the supplied
// transact helper), the delta is NOT captured and Apply returns
// ErrNoChanges even though the doc has been mutated. This is a
// contract violation, but the behavior is well-defined.
//
// NOTE: a panic inside fn propagates to the caller. The OnUpdate
// subscription is cleaned up via defer, so no listener leaks. However,
// due to a pre-existing bug in crdt.Doc.Transact, a panic inside fn's
// transaction also leaks the doc's write lock, wedging the room.
// Callers MUST ensure fn does not panic.
func (s *Server) Apply(
	ctx context.Context,
	room string,
	fn func(doc *crdt.Doc, transact func(func(*crdt.Transaction))),
) error {
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
	if s.OnInject != nil {
		if err := s.OnInject(ctx, InjectInfo{
			Room:       room,
			Op:         OpApply,
			UpdateSize: 0,
		}); err != nil {
			return fmt.Errorf("%w: %w", ErrInjectRefused, err)
		}
	}
	rm, err := s.getOrCreateRoom(room)
	if err != nil {
		return err
	}

	origin := new(struct{})
	var (
		captured   [][]byte
		capturedMu sync.Mutex
	)
	unsub := rm.doc.OnUpdate(func(update []byte, o any) {
		if o != origin {
			return
		}
		// Mutex guards against the (unusual but legal) case where fn
		// spawns a goroutine that calls transact() concurrently with
		// the main fn body. Also guards against deep-observer chains
		// that re-enter transact.
		capturedMu.Lock()
		captured = append(captured, update)
		capturedMu.Unlock()
	})
	defer unsub()

	transact := func(inner func(*crdt.Transaction)) {
		rm.doc.Transact(inner, origin)
	}
	fn(rm.doc, transact)

	capturedMu.Lock()
	capturedCopy := make([][]byte, len(captured))
	copy(capturedCopy, captured)
	capturedMu.Unlock()

	if len(capturedCopy) == 0 {
		return ErrNoChanges
	}

	var merged []byte
	if len(capturedCopy) == 1 {
		merged = capturedCopy[0]
	} else {
		m, err := crdt.MergeUpdatesV1(capturedCopy...)
		if err != nil {
			return fmt.Errorf("ygo/websocket: merging captured updates: %w", err)
		}
		merged = m
	}
	if len(merged) > s.effectiveMaxUpdateBytes() {
		return ErrUpdateTooLarge
	}

	rm.mu.Lock()
	targets := make([]*peer, 0, len(rm.peers))
	for p := range rm.peers {
		targets = append(targets, p)
	}
	rm.mu.Unlock()

	data := encodeBroadcastWire(merged)
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
