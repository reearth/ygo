package websocket

import (
	"errors"
	"fmt"
	"sync"

	"github.com/reearth/ygo/crdt"
)

// PeerUpdateInfo is passed to Server.OnPeerUpdate for each SyncStep2 or
// Update a read-write peer sends. Fields may be added in future versions;
// callers must not rely on the struct being fixed-size.
type PeerUpdateInfo struct {
	// Room is the room name the update arrived on.
	Room string
	// Doc is the room's document, which apply applies the update to.
	Doc *crdt.Doc
	// Update is the update's V1 bytes, the content of the sync message.
	// The hook must not modify or retain it.
	Update []byte
	// Origin is the transaction origin apply applies the update with, which
	// the document's OnUpdate observers receive.
	Origin any
}

// PeerUpdateHook is the signature of Server.OnPeerUpdate. apply applies
// info.Update to info.Doc as the room does without a hook, returning the
// apply's error (an update whose V1 bytes do not decode, or one that
// overflows MaxPendingItems); a second call returns ErrPeerUpdateApplied
// without applying anything. apply must be called before the hook returns: a
// call after it returns applies nothing and returns ErrPeerUpdateHookReturned,
// since by then the peer has been answered and the update would reach no other
// peer. The hook runs on the peer's read loop, so the peer's next frame waits
// for it. apply takes the document's write lock, so calling it inside
// info.Doc.Transact deadlocks the room.
//
// A hook that refuses the update — returns without apply having applied it,
// or panics before that — closes the peer: with the code and reason of a
// *PeerClose when the hook's error is or wraps one, and with 4205 "Reset
// Connection" otherwise, as @hocuspocus/server closes on a beforeHandleMessage
// rejection. When apply ran and the room itself did not take the update
// (apply returned an error), the peer is answered as it would be without a
// hook, and closed only for a *PeerClose.
//
// The close is needed because the client's later edits build on the refused
// one, so the room could not integrate them. The client still holds the
// refused edit, so when it reconnects it sends it again, with any edit built
// on it, and the hook sees each of them again; an edit the hook applies that
// builds on a refused one parks in the room (Doc.PendingStats).
// @hocuspocus/provider 2.x reconnects at once after a 4205 close; a
// *PeerClose with 4401 stops it reconnecting.
type PeerUpdateHook func(info PeerUpdateInfo, apply func() error) error

// ErrPeerUpdateApplied is what apply returns when the hook calls it again
// after it already ran.
var ErrPeerUpdateApplied = errors.New("ygo/websocket: peer update already applied")

// ErrPeerUpdateHookReturned is what apply returns when it is called after the
// hook returned. It applies nothing.
var ErrPeerUpdateHookReturned = errors.New("ygo/websocket: apply called after OnPeerUpdate returned")

// PeerClose is an error Server.OnPeerUpdate returns to close the peer whose
// update it refused, with Code and Reason in the close frame. Code should be
// an application code (4000-4999). Only 1000 and 3000-4999 are sent, the rule
// ServeHTTP applies to a context cause's close code: the codes below 3000
// other than 1000 are reserved for the WebSocket protocol and its
// implementations, so with one (or 5000 and above) the peer is closed with
// 4205 "Reset Connection" instead, as for a plain error. A nil *PeerClose is a
// plain error too. Invalid UTF-8 is dropped from Reason, and a Reason longer
// than a close frame carries (123 bytes) is cut at a rune boundary.
type PeerClose struct {
	Code   int
	Reason string
}

func (c *PeerClose) Error() string {
	if c == nil {
		return "ygo/websocket: close peer: <nil>"
	}
	return fmt.Sprintf("ygo/websocket: close peer with %d: %s", c.Code, c.Reason)
}

// applyPeerUpdate applies a read-write peer's update to the room, through
// Server.OnPeerUpdate when one is set. It reports whether the room applied
// the update and, when the peer is to be closed, the close's sendable code and
// reason (see PeerUpdateHook). A hook that panics is recovered and logged; the
// update stands applied only if the hook's apply applied it.
func (p *peer) applyPeerUpdate(update []byte) (applied bool, closeWith *PeerClose) {
	hook := p.server.OnPeerUpdate
	if hook == nil {
		if err := p.applyToRoom(update); err != nil {
			p.server.log().Debug("discarded unappliable sync message",
				"room", p.roomName, "err", err)
			return false, nil
		}
		return true, nil
	}
	// mu orders apply's start against the hook's return: a call that started
	// first counts, and one after the return applies nothing. It is not held
	// while the update applies, so a call made while the first one runs (from a
	// document observer, say) returns ErrPeerUpdateApplied instead of blocking.
	var (
		mu               sync.Mutex
		called, returned bool
		applyErr         error
		applyDone        = make(chan struct{})
	)
	apply := func() error {
		mu.Lock()
		switch {
		case returned:
			mu.Unlock()
			return ErrPeerUpdateHookReturned
		case called:
			mu.Unlock()
			return ErrPeerUpdateApplied
		}
		called = true
		mu.Unlock()
		defer close(applyDone)
		applyErr = p.applyToRoom(update)
		return applyErr
	}
	var hookErr error
	p.server.safeHook("OnPeerUpdate", func() {
		hookErr = hook(PeerUpdateInfo{Room: p.roomName, Doc: p.room.doc, Update: update, Origin: p}, apply)
	})
	mu.Lock()
	returned = true
	ranApply := called
	mu.Unlock()
	if ranApply {
		<-applyDone // an apply that started before the return finishes first
	}
	if ranApply && applyErr == nil {
		return true, nil
	}
	p.server.log().Debug("peer update refused", "room", p.roomName,
		"hookErr", hookErr, "applyErr", applyErr)
	var refusal *PeerClose
	if errors.As(hookErr, &refusal) && refusal != nil {
		if code, reason, ok := sendableClose(refusal.Code, refusal.Reason); ok {
			return false, &PeerClose{Code: code, Reason: reason}
		}
	} else if ranApply {
		// The room did not take the update: answered as without a hook.
		return false, nil
	}
	return false, &PeerClose{Code: wsCodeResetConnection, Reason: wsReasonResetConnection}
}
