package websocket

import (
	"errors"
	"fmt"
	"unicode/utf8"

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
// without applying anything. The hook runs on the peer's read loop, so the
// peer's next frame waits for it.
type PeerUpdateHook func(info PeerUpdateInfo, apply func() error) error

// ErrPeerUpdateApplied is what apply returns when the hook calls it again
// after it already ran.
var ErrPeerUpdateApplied = errors.New("ygo/websocket: peer update already applied")

// PeerClose is an error Server.OnPeerUpdate returns to close the peer whose
// update it refused, with Code and Reason in the close frame. Code should be
// an application code (4000-4999). A Reason longer than a close frame carries
// (123 bytes) is cut at a rune boundary.
type PeerClose struct {
	Code   int
	Reason string
}

func (c *PeerClose) Error() string {
	return fmt.Sprintf("ygo/websocket: close peer with %d: %s", c.Code, c.Reason)
}

// maxCloseReasonBytes is the most a close frame's reason holds: a control
// frame's 125-byte payload less the two-byte code.
const maxCloseReasonBytes = 123

// closeReason is reason cut to the last whole rune within maxCloseReasonBytes.
func closeReason(reason string) string {
	if len(reason) <= maxCloseReasonBytes {
		return reason
	}
	cut := maxCloseReasonBytes
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

// applyPeerUpdate applies a read-write peer's update to the room, through
// Server.OnPeerUpdate when one is set. It reports whether the room applied
// the update, and the *PeerClose the hook refused it with, if any. A hook
// that panics is recovered and logged; the update stands applied only if the
// hook's apply ran.
func (p *peer) applyPeerUpdate(update []byte) (applied bool, closeWith *PeerClose) {
	hook := p.server.OnPeerUpdate
	if hook == nil {
		if err := crdt.ApplyUpdateV1(p.room.doc, update, p); err != nil {
			p.server.log().Debug("discarded unappliable sync message",
				"room", p.roomName, "err", err)
			return false, nil
		}
		return true, nil
	}
	called := false
	apply := func() error {
		if called {
			return ErrPeerUpdateApplied
		}
		called = true
		if err := crdt.ApplyUpdateV1(p.room.doc, update, p); err != nil {
			return err
		}
		applied = true
		return nil
	}
	var hookErr error
	p.server.safeHook("OnPeerUpdate", func() {
		hookErr = hook(PeerUpdateInfo{Room: p.roomName, Doc: p.room.doc, Update: update, Origin: p}, apply)
	})
	if applied {
		return true, nil
	}
	if hookErr != nil {
		p.server.log().Debug("peer update refused", "room", p.roomName, "err", hookErr)
		var refusal *PeerClose
		if errors.As(hookErr, &refusal) {
			return false, refusal
		}
	}
	return false, nil
}
