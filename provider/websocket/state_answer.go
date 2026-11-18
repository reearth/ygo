package websocket

import (
	"errors"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"
)

// StateAnswerInfo is passed to Server.OnStateAnswer for each SyncStep1 the
// server is about to answer with the room's state. Fields may be added in
// future versions; callers must not rely on the struct being fixed-size.
type StateAnswerInfo struct {
	// Room is the room name the SyncStep1 arrived on.
	Room string
	// Doc is the room's document, which the answer reads once the hook
	// returns. The hook must not write to it: a write that does not go
	// through Server.Apply is broadcast to no peer.
	Doc *crdt.Doc
	// Structs is how many structs Doc holds, those parked in its pending
	// queue included. It is the most structs the answer carries, and the
	// answer to a peer that holds nothing yet carries all of them. It is read
	// just before the hook is called; a hook that waits before it admits the
	// answer reads StructsNow.
	Structs int
	// Step1 is the SyncStep1 message the peer sent. The hook must not modify
	// or retain it.
	Step1 []byte
}

// StructsNow is Structs read again. The answer is built from the room as it
// is when the hook returns, which a hook that waited sees only here.
func (i StateAnswerInfo) StructsNow() int {
	return answerStructs(i.Doc)
}

// answerStructs counts parked structs too, because the answer carries them
// (crdt.SplitStateAsUpdateV1). A nil doc holds no structs.
func answerStructs(doc *crdt.Doc) int {
	if doc == nil {
		return 0
	}
	return doc.StructCount() + doc.PendingStats().Items
}

// StateAnswerHook is the signature of Server.OnStateAnswer. It runs on the
// peer's read loop just before the server encodes its answer to the peer's
// SyncStep1, and the release it returns runs once the server is done with
// that answer.
//
// Each answer costs the server memory in proportion to what the peer is
// missing, however short the SyncStep1 that asked for it: the server builds
// every frame of the answer before it queues them (sync.EncodeSyncStep2Split).
// The answers to peers that join at the same time are built at the same time.
// The hook is where a server admits an answer, makes it wait, or refuses it.
//
// A nil error admits the answer. A non-nil release then runs exactly once:
// after the server has handed the answer's frames to the peer's write queue,
// which Server.PeerWriteQueueBytes bounds, or after it failed to encode the
// answer and sent nothing, which it logs at Debug as it does without a hook.
// A release that panics is recovered and logged.
//
// A non-nil error refuses the answer: nothing is sent, the release is not
// run, and the peer is closed, with the code and reason of a *PeerClose the
// error is or wraps and with 4205 "Reset Connection" otherwise, as for a
// refused peer update (PeerUpdateHook). A hook that panics is recovered,
// logged, and refuses. The peer is closed because a client whose SyncStep1
// is never answered waits for that answer for as long as it stays connected;
// @hocuspocus/provider 2.x reconnects at once after a 4205 close.
//
// The peer's next frame waits for the hook and its release, and so do the
// peer's teardown and Server.Shutdown, so a hook that waits must bound its
// wait.
type StateAnswerHook func(info StateAnswerInfo) (release func(), err error)

// errStateAnswerPanic stands in for the error of a hook that panicked, which
// refuses the answer like any other error.
var errStateAnswerPanic = errors.New("ygo/websocket: OnStateAnswer panicked")

// answerSyncStep1 answers step1, a peer's SyncStep1 message, with the room's
// state, through Server.OnStateAnswer when one is set.
func (p *peer) answerSyncStep1(step1 []byte) {
	release, closeWith := p.admitStateAnswer(step1)
	if closeWith != nil {
		p.enqueueClose(closeWith.Code, closeWith.Reason)
		return
	}
	if release != nil {
		defer p.server.safeHook("OnStateAnswer release", release)
	}
	reply, err := ygsync.EncodeSyncStep2Split(p.room.doc, step1, p.stateMessageBytes())
	if err != nil {
		p.server.log().Debug("discarded unappliable sync message",
			"room", p.roomName, "err", err)
		return
	}
	p.sendSyncSeries(reply)
}

// admitStateAnswer runs Server.OnStateAnswer for the answer to step1. It
// returns the release to run once the answer is sent and, when the hook
// refused the answer, the close's sendable code and reason (see
// StateAnswerHook). With no hook it returns neither.
func (p *peer) admitStateAnswer(step1 []byte) (release func(), closeWith *PeerClose) {
	hook := p.server.OnStateAnswer
	if hook == nil {
		return nil, nil
	}
	info := StateAnswerInfo{
		Room:    p.roomName,
		Doc:     p.room.doc,
		Structs: answerStructs(p.room.doc),
		Step1:   step1,
	}
	hookErr := errStateAnswerPanic
	p.server.safeHook("OnStateAnswer", func() { release, hookErr = hook(info) })
	if hookErr == nil {
		return release, nil
	}
	p.server.log().Debug("state answer refused", "room", p.roomName, "err", hookErr)
	var refusal *PeerClose
	if errors.As(hookErr, &refusal) && refusal != nil {
		if code, reason, ok := sendableClose(refusal.Code, refusal.Reason); ok {
			return nil, &PeerClose{Code: code, Reason: reason}
		}
	}
	return nil, &PeerClose{Code: wsCodeResetConnection, Reason: wsReasonResetConnection}
}
