package websocket

// enqueueLocked hands data to the peer's writer and charges it to the peer's
// Server.PeerWriteQueueBytes. It reports true only when writeCh was full, so
// the caller can run its own full-queue handling. When the frames waiting for
// the peer already reach its budget, the frame is not queued and the peer is
// closed, whatever the SlowPeerPolicy, because a peer over its budget drains
// only if it reads. The caller holds wmu and has checked closed.
func (p *peer) enqueueLocked(data []byte, path string) (full bool) {
	if p.overBudgetLocked() {
		p.server.log().Warn("peer queued bytes over budget; closing peer",
			"room", p.roomName,
			"path", path,
			"queuedBytes", p.queuedBytes,
			"budgetBytes", p.writeBudget,
			"frameBytes", len(data),
			"queueSize", cap(p.writeCh))
		_ = p.conn.Close()
		return false
	}
	select {
	case p.writeCh <- data:
		switch {
		case p.writeBudget <= 0:
		case p.handshaking:
			p.unchargedFrames++
		default:
			p.queuedBytes += int64(len(data))
		}
		return false
	default:
		return true
	}
}

// overBudgetLocked reports whether the frames waiting for the peer reach its
// budget. A frame is refused only then, so the frame that crosses the budget
// is queued whatever its size. The caller holds wmu.
func (p *peer) overBudgetLocked() bool {
	return p.writeBudget > 0 && !p.handshaking && p.queuedBytes >= p.writeBudget
}

// creditLocked takes a frame runWriter has taken off writeCh out of the
// count. writeCh is first in, first out, so the frames queued during the
// handshake, which were never charged, leave it first. The caller holds wmu.
func (p *peer) creditLocked(n int) {
	if p.unchargedFrames > 0 {
		p.unchargedFrames--
		return
	}
	p.queuedBytes -= int64(n)
}

// setHandshaking marks the frames queued while on is true as the handshake,
// which is never refused or charged: a peer cannot ask for it again, and a
// client may ask for the room's state before it reads it.
func (p *peer) setHandshaking(on bool) {
	if p.writeBudget <= 0 {
		return
	}
	p.wmu.Lock()
	p.handshaking = on
	p.wmu.Unlock()
}
