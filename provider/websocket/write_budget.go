package websocket

// enqueueLocked queues data and charges it to the peer's byte budget, closing
// a peer already over budget on a non-handshake frame because such a peer
// drains only if it reads.
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
// reach its budget, so the frame that crosses it is still queued.
func (p *peer) overBudgetLocked() bool {
	return p.writeBudget > 0 && p.queuedBytes >= p.writeBudget
}

// creditLocked uncharges the next frame runWriter took off writeCh unless it
// is a handshake frame, relying on wmu to keep frames in enqueue order.
func (p *peer) creditLocked(n int) {
	seq := p.dequeued
	p.dequeued++
	if seq < p.handshakeFrom || seq >= p.handshakeTo {
		p.queuedBytes -= int64(n)
	}
}

// writeHandshake queues a peer's connect frames back to back and uncharged,
// because a peer cannot ask for them again.
func (p *peer) writeHandshake(frames ...[]byte) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.handshakeFrom = p.enqueued
	for _, f := range frames {
		p.writeLocked(f, true)
	}
	p.handshakeTo = p.enqueued
}
