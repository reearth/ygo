package websocket

import (
	"context"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// PersistenceAdapterStored is an optional extension to PersistenceAdapter
// for Server.AnswerOnceStored. StoreUpdateStored is StoreUpdateContext that
// also says whether the update is stored once the call returns: an adapter
// may return nil for an update it dropped (one of a room it has given up on,
// say), which AnswerOnceStored must not answer as stored. Under
// AnswerOnceStored the persistence worker calls it, when the adapter
// implements it, in place of StoreUpdateContext and StoreUpdate; otherwise an
// update is stored when the call that took it returned nil. Without the
// option it is never called.
type PersistenceAdapterStored interface {
	StoreUpdateStored(ctx context.Context, room string, update []byte) (stored bool, err error)
}

// answersOnceStored is whether a peer's answers wait for the store: under
// AnswerOnceStored, on a server with persistence. Without persistence nothing
// stores an update, and every answer goes out as without the option.
func (s *Server) answersOnceStored() bool {
	return s.AnswerOnceStored && s.persistence != nil
}

// storeOne is one adapter call for update, the call the persistence worker
// makes, and whether it stored update: under AnswerOnceStored,
// StoreUpdateStored's answer where the adapter implements it, and otherwise a
// nil error.
func (s *Server) storeOne(ctx context.Context, name string, update []byte) (bool, error) {
	if st, ok := s.persistence.(PersistenceAdapterStored); ok && s.answersOnceStored() {
		return st.StoreUpdateStored(ctx, name, update)
	}
	var err error
	if pac, ok := s.persistence.(PersistenceAdapterContext); ok {
		err = pac.StoreUpdateContext(ctx, name, update)
	} else {
		err = s.persistence.StoreUpdate(name, update)
	}
	return err == nil, err
}

// updateKey is what a commit's store is found by: the first byte of the very
// update slice the room's OnUpdate observers were handed, which the
// persistence worker is handed too. Until its store is known the update is
// held by the worker's queue, so no other update has that address.
func updateKey(update []byte) *byte { return unsafe.SliceData(update) }

// noteStored records whether update, a commit of r, was stored, and sends
// the answers that waited for it (roomStores). A second call for the same
// update finds nothing. It runs on the persistence worker's goroutine, or on
// the committing one for a retired room's write (persistStranded).
func (s *Server) noteStored(r *room, update []byte, stored bool) {
	if !s.answersOnceStored() || len(update) == 0 {
		return
	}
	for _, then := range r.stores.settle(update, stored) {
		then()
	}
}

// armStore is the first step of the room's persistence observer, which runs
// on the committing goroutine before the update reaches the worker: every
// commit of the room, whoever made it, is a store the answers wait for.
func (s *Server) armStore(r *room, update []byte) {
	if !s.answersOnceStored() || len(update) == 0 {
		return
	}
	r.stores.arm(update)
}

// roomStores is a room instance's record, under AnswerOnceStored, of the
// stores of its commits: each commit the room's persistence observer sees is
// numbered (arm), and settled when the worker reports its store (settle). A
// peer's answer of 1 waits until every commit numbered when the answer was
// decided is settled (after), and goes out as 0 once a commit of the room
// instance was not stored (unstored). So a frame whose apply commits
// nothing - a SyncStep2 or a resend of what the room holds - takes the
// verdict of the newest store still running, whichever peer's commit it
// stores, as a frame that commits takes its own; and an edit the room holds
// only because a commit that was not stored put it there is never answered
// applied. A room loaded again is a new instance and starts clean.
type roomStores struct {
	mu       sync.Mutex
	numbered uint64               // commits numbered so far
	open     map[*byte]*openStore // the commits not yet settled, by updateKey
	queue    []*openStore         // the same and settled ones, in number order, from the oldest not settled
	waits    []storesWait         // answers waiting, in the order of through
	unstored atomic.Bool          // a commit of the room instance was not stored
}

// openStore is one commit whose store the answers wait for.
type openStore struct {
	n       uint64
	settled bool
}

// storesWait is an answer waiting until every commit up to through is settled.
type storesWait struct {
	through uint64
	then    func()
}

// arm numbers update, a commit of the room.
func (rs *roomStores) arm(update []byte) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.numbered++
	o := &openStore{n: rs.numbered}
	if rs.open == nil {
		rs.open = make(map[*byte]*openStore)
	}
	rs.open[updateKey(update)] = o
	rs.queue = append(rs.queue, o)
}

// settle records whether update was stored and returns the answers it
// releases, for the caller to send once the record's lock is free.
func (rs *roomStores) settle(update []byte, stored bool) []func() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	o, ok := rs.open[updateKey(update)]
	if !ok {
		return nil
	}
	delete(rs.open, updateKey(update))
	o.settled = true
	if !stored {
		rs.unstored.Store(true)
	}
	for len(rs.queue) > 0 && rs.queue[0].settled {
		rs.queue[0] = nil
		rs.queue = rs.queue[1:]
	}
	through := rs.numbered
	if len(rs.queue) > 0 {
		through = rs.queue[0].n - 1
	}
	var ready []func()
	for len(rs.waits) > 0 && rs.waits[0].through <= through {
		ready = append(ready, rs.waits[0].then)
		rs.waits[0] = storesWait{}
		rs.waits = rs.waits[1:]
	}
	return ready
}

// after runs then once every commit numbered so far is settled, at once when
// every one is.
func (rs *roomStores) after(then func()) {
	rs.mu.Lock()
	if len(rs.queue) == 0 {
		rs.mu.Unlock()
		then()
		return
	}
	rs.waits = append(rs.waits, storesWait{through: rs.numbered, then: then})
	rs.mu.Unlock()
}

// answerOnceStored resolves a, a frame's SyncStatus, with applied and
// closeWith once every commit of the room numbered by now is stored or
// known not to be (roomStores.after): the frame's own commit when it made
// one, and every other commit that may have put in the room what the frame
// carries. An answer of 0 needs no store, and is ready at once.
func (p *peer) answerOnceStored(a *peerAnswer, applied bool, closeWith *PeerClose) {
	if !applied {
		p.resolveAnswer(a, false, closeWith)
		return
	}
	p.room.stores.after(func() { p.resolveAnswer(a, true, closeWith) })
}

// peerAnswers is what a peer is owed under AnswerOnceStored, in the order its
// frames arrived: each frame's SyncStatus, a close the server decided on (a
// hook's refusal, Server.ClosePeer), and the room's SyncStep1
// (Server.ResyncPeer). Each goes out once it is ready and everything before
// it has gone out.
type peerAnswers struct {
	mu    sync.Mutex
	queue []*peerAnswer
}

// peerAnswer is one entry: a frame's SyncStatus (status, applied), a close,
// or both, ready once the stores it waits for are known; or a frame to
// write, ready once it is built.
type peerAnswer struct {
	ready, status, applied bool
	close                  *PeerClose
	frame                  []byte
}

// reserveAnswer takes the next place in p's answers for a frame's SyncStatus.
func (p *peer) reserveAnswer() *peerAnswer {
	return p.reserve(&peerAnswer{status: true})
}

// reserve takes the next place in p's answers for a.
func (p *peer) reserve(a *peerAnswer) *peerAnswer {
	p.answers.mu.Lock()
	p.answers.queue = append(p.answers.queue, a)
	p.answers.mu.Unlock()
	return a
}

// resolveAnswer makes a ready with its answer and close, and sends every
// ready entry at the head of p's answers, in order (sendReadyLocked).
func (p *peer) resolveAnswer(a *peerAnswer, applied bool, closeWith *PeerClose) {
	p.answers.mu.Lock()
	defer p.answers.mu.Unlock()
	a.ready, a.applied, a.close = true, applied, closeWith
	p.sendReadyLocked()
}

// resolveFrame makes a ready with frame to write, and sends every ready
// entry at the head of p's answers, in order (sendReadyLocked).
func (p *peer) resolveFrame(a *peerAnswer, frame []byte) {
	p.answers.mu.Lock()
	defer p.answers.mu.Unlock()
	a.ready, a.frame = true, frame
	p.sendReadyLocked()
}

// sendReadyLocked sends every ready entry at the head of p's answers, in
// order, with the answers' lock held, so two goroutines resolving at once
// cannot send out of order. A SyncStatus goes out as 0 once a commit of the
// room instance was not stored (roomStores.unstored), whatever it was
// resolved with: the room then holds content no store holds, which the
// frame may carry.
func (p *peer) sendReadyLocked() {
	q := &p.answers
	for len(q.queue) > 0 && q.queue[0].ready {
		head := q.queue[0]
		q.queue[0] = nil
		q.queue = q.queue[1:]
		if head.status {
			p.sendSyncStatus(head.applied && !p.room.stores.unstored.Load())
		}
		if head.frame != nil {
			p.write(head.frame)
		}
		if head.close != nil {
			p.closeFromAnyGoroutine(head.close.Code, head.close.Reason)
		}
	}
}

// closeFromAnyGoroutine is enqueueClose for a caller on any goroutine. It
// holds wmu across the send of the close sentinel, as write holds it across
// its send, so the send cannot race handleDisconnect's close of writeCh, and
// sends the close frame itself, once wmu is released, when the queue is full.
// It reports whether it queued or sent the close: false for a peer that is
// closed or already closing.
func (p *peer) closeFromAnyGoroutine(code int, reason string) bool {
	p.inputClosed.Store(true)
	p.wmu.Lock()
	if p.closed || p.closeCode != 0 {
		p.wmu.Unlock()
		return false
	}
	p.closeCode, p.closeReason = code, reason
	queued := false
	select {
	case p.writeCh <- nil:
		queued = true
	default:
	}
	p.wmu.Unlock()
	if !queued {
		p.sendCloseFrame()
	}
	return true
}

// ClosePeer closes the peer whose updates carry origin as their transaction
// origin - PeerUpdateInfo.Origin, the origin the room's document records for
// what that peer's updates did - with code and reason, and reports whether
// origin was a peer of s still open. It may be called from any
// goroutine, an OnPeerUpdate hook included. A code ServeHTTP would not send
// (see PeerClose) closes with 4205 "Reset Connection" instead, and a reason
// is cut as a PeerClose's is. Nothing the peer sends from then on lands.
//
// Under AnswerOnceStored the close goes out after every SyncStatus the peer
// is owed for frames it sent before the call, once those are answered, so a
// client reads the answers to its frames before the close; otherwise it goes
// out behind what the peer's write queue holds, as a hook's refusal does.
func (s *Server) ClosePeer(origin any, code int, reason string) bool {
	p, ok := s.openPeer(origin)
	if !ok {
		return false
	}
	if c, r, sendable := sendableClose(code, reason); sendable {
		code, reason = c, r
	} else {
		code, reason = wsCodeResetConnection, wsReasonResetConnection
	}
	p.inputClosed.Store(true)
	if s.answersOnceStored() {
		p.resolveAnswer(p.reserve(&peerAnswer{}), false, &PeerClose{Code: code, Reason: reason})
		return true
	}
	return p.closeFromAnyGoroutine(code, reason)
}

// ResyncPeer sends the peer whose updates carry origin as their transaction
// origin, as for ClosePeer, the room's SyncStep1, which states the room's
// state vector, and reports whether origin was a peer of s still open. A
// provider answers a SyncStep1 with a SyncStep2 carrying what it holds and
// the room lacks, and the room applies and answers that as any other update.
// So a caller that sees the room land content a peer's frame parked can have
// the peer send its state again and be answered for it.
//
// It may be called from any goroutine, with the room's document locked too
// (from inside a Transact on it, or from an observer that runs under its
// lock): the state vector is read on a goroutine of its own once the
// document's lock is free, so the SyncStep1 states the room as it is then.
// Under AnswerOnceStored the SyncStep1 goes out after every answer the peer
// is owed for frames it sent before the call, once those are answered;
// otherwise it goes out behind what the peer's write queue holds when it is
// read, the answers to those frames included.
func (s *Server) ResyncPeer(origin any) bool {
	p, ok := s.openPeer(origin)
	if !ok {
		return false
	}
	var a *peerAnswer
	if s.answersOnceStored() {
		a = p.reserve(&peerAnswer{})
	}
	go func() {
		frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarUint(msgSync)
			enc.WriteRaw(ygsync.EncodeSyncStep1(p.room.doc)) // sync payload is NOT VarBytes-wrapped
		})
		if a != nil {
			p.resolveFrame(a, frame)
			return
		}
		p.write(frame)
	}()
	return true
}

// openPeer is origin as a peer of s that is neither closed nor closing.
func (s *Server) openPeer(origin any) (*peer, bool) {
	p, ok := origin.(*peer)
	if !ok || p.server != s {
		return nil, false
	}
	p.wmu.Lock()
	open := !p.closed && p.closeCode == 0
	p.wmu.Unlock()
	return p, open
}
