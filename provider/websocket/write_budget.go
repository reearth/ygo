package websocket

// enqueueLocked hands data to the peer's writer and charges it to the peer's
// Server.PeerWriteQueueBytes, unless it is a handshake frame. It reports true
// only when writeCh was full, so the caller can run its own full-queue
// handling; it reports false also when it refused the frame. It refuses a
// frame, other than a handshake frame, when the frames waiting for the peer
// already reach its budget, and then closes the peer whatever the
// SlowPeerPolicy, because a peer over its budget drains only if it reads.
// The read loop drops the input the peer already sent, so it builds no more
// answers for it. The caller holds wmu and has checked closed.
func (p *peer) enqueueLocked(data []byte, path string, handshake bool) (full bool) {
	if !handshake && p.overBudgetLocked() {
		p.server.log().Warn("peer queued bytes over budget; closing peer",
			"room", p.roomName,
			"path", path,
			"queuedBytes", p.queuedBytes,
			"budgetBytes", p.writeBudget,
			"frameBytes", len(data),
			"queueSize", cap(p.writeCh))
		p.inputClosed.Store(true)
		_ = p.conn.Close()
		return false
	}
	select {
	case p.writeCh <- data:
		if p.writeBudget > 0 {
			p.enqueued++
			if !handshake {
				p.queuedBytes += int64(len(data))
			}
		}
		return false
	default:
		return true
	}
}

// overBudgetLocked reports whether the charged frames waiting for the peer
// reach its budget. A frame is refused only then, so the frame that crosses
// the budget is queued whatever its size. The caller holds wmu.
func (p *peer) overBudgetLocked() bool {
	return p.writeBudget > 0 && p.queuedBytes >= p.writeBudget
}

// creditLocked takes the next frame runWriter took off writeCh out of the
// count, unless it is a handshake frame. Every data frame is sent on writeCh
// under wmu, so frames leave it in the order enqueued numbered them. The nil
// close sentinel is sent without wmu, but it is not numbered and runWriter
// stops at it. The caller holds wmu.
func (p *peer) creditLocked(n int) {
	seq := p.dequeued
	p.dequeued++
	if seq < p.handshakeFrom || seq >= p.handshakeTo {
		p.queuedBytes -= int64(n)
	}
}

// writeHandshake queues the frames a peer gets when it connects, with no
// other frame between them. Server.PeerWriteQueueBytes neither refuses nor
// charges them: a peer cannot ask for them again, and a client may ask for
// the room's state before it reads them.
func (p *peer) writeHandshake(frames ...[]byte) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.handshakeFrom = p.enqueued
	for _, f := range frames {
		p.writeLocked(f, true)
	}
	p.handshakeTo = p.enqueued
}
