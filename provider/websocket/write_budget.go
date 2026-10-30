package websocket

// enqueueLocked hands data to the peer's writer and charges it to the peer's
// Server.PeerWriteQueueBytes. It reports whether writeCh was full, so the
// caller can run its own full-queue handling. A frame the budget refuses is
// not queued, and the peer is closed whatever the SlowPeerPolicy, because a
// peer over its budget drains only if it reads. The caller holds wmu and has
// checked closed.
func (p *peer) enqueueLocked(data []byte, path string) (full bool) {
	if !p.withinBudget(len(data)) {
		p.server.log().Warn("peer queued bytes over budget; closing peer",
			"room", p.roomName,
			"cause", CloseCauseWriteQueueBytes,
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
		if p.writeBudget > 0 {
			p.queuedBytes += int64(len(data))
		}
		return false
	default:
		return true
	}
}

// withinBudget reports whether a frame of n bytes may be queued. Any frame may
// before the handshake is queued, and on an empty queue. The caller holds wmu.
func (p *peer) withinBudget(n int) bool {
	if p.writeBudget <= 0 || !p.budgetArmed || p.queuedBytes == 0 {
		return true
	}
	return p.queuedBytes+int64(n) <= p.writeBudget
}

// armBudget lets the budget refuse frames. ServeHTTP calls it once the
// handshake is queued: a peer cannot ask for the handshake again, and a
// budget smaller than the room's state must not stop it joining.
func (p *peer) armBudget() {
	if p.writeBudget <= 0 {
		return
	}
	p.wmu.Lock()
	p.budgetArmed = true
	p.wmu.Unlock()
}
