package websocket

// enqueueLocked queues data and charges it to the peer's byte budget, closing
// a peer already over budget on a non-handshake frame because such a peer
// drains only if it reads.
func (p *peer) enqueueLocked(data []byte, path string, handshake bool) (full bool) {
	if !handshake && p.overBudgetLocked() {
		p.closeOverBudgetLocked(path, len(data))
		return false
	}
	return p.pushLocked(data, handshake)
}

// closeOverBudgetLocked closes a peer whose charged frames reach its budget,
// naming the frame that found it over and the path that offered it.
func (p *peer) closeOverBudgetLocked(path string, frameBytes int) {
	p.server.log().Warn("peer queued bytes over budget; closing peer",
		"room", p.roomName,
		"path", path,
		"queuedBytes", p.queuedBytes,
		"budgetBytes", p.writeBudget,
		"frameBytes", frameBytes,
		"queueSize", cap(p.writeCh))
	p.closeQueueLocked()
}

// pushLocked queues data, charging its bytes unless it is a handshake frame,
// and reports a full queue without closing the peer.
func (p *peer) pushLocked(data []byte, handshake bool) (full bool) {
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

// writeSeries queues frames back to back, so no frame the peer is sent later
// lands between them, and charges them to the byte budget on the one decision
// a single frame of the same bytes would have got: a peer already over budget
// is closed and none of them is queued, and a peer under it takes them all.
// One answer stays one answer however many frames it is cut into.
func (p *peer) writeSeries(frames ...[]byte) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.closed || p.queueClosed {
		return
	}
	if p.overBudgetLocked() {
		bytes := 0
		for _, f := range frames {
			bytes += len(f)
		}
		p.closeOverBudgetLocked("direct send", bytes)
		return
	}
	for _, f := range frames {
		if full := p.pushLocked(f, false); full {
			p.server.log().Warn("peer write queue full during direct send; closing peer",
				"room", p.roomName,
				"queueSize", cap(p.writeCh))
			p.closeQueueLocked()
			return
		}
	}
}
