package websocket

import (
	"context"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// peer is one connected WebSocket client.
type peer struct {
	conn       *gws.Conn
	wmu        sync.Mutex // serialises concurrent writes
	closed     bool       // H2: true after handleDisconnect; guarded by wmu
	room       *room
	roomName   string              // C1: name used to delete room when empty
	server     *Server             // C1: back-reference for room map cleanup
	done       chan struct{}       // H1: closed when the read loop exits
	clientIDs  map[uint64]struct{} // awareness clientIDs controlled by this peer
	cidMu      sync.Mutex
	writeCh    chan []byte   // buffered queue drained by runWriter goroutine
	writerDone chan struct{} // closed when runWriter exits
	limiter    *rate.Limiter // per-peer inbound-message rate limiter; nil = unlimited (#51)

	// disconnectOnce ensures the full teardown sequence in handleDisconnect
	// runs exactly once, regardless of how many callers race (e.g. broadcast's
	// conn.Close() triggering the read loop while a ctx-cancel path also calls
	// handleDisconnect). The closed bool under wmu is still needed for the
	// per-operation guards in broadcast/write/runWriter.
	disconnectOnce sync.Once
}

// handleMessage decodes the outer message type and dispatches accordingly.
func (p *peer) handleMessage(data []byte) {
	dec := encoding.NewDecoder(data)
	outerType, err := dec.ReadVarUint()
	if err != nil {
		// Debug, not Warn: the rate is attacker-controlled, so a noisier level
		// would be a log-flood vector. Still logged so an operator can see that
		// frames are being discarded (N-12).
		p.server.log().Debug("discarded malformed message: unreadable outer type",
			"room", p.roomName, "err", err)
		return
	}

	switch outerType {
	case msgSync:
		// Sync payload follows directly (no VarBytes wrapper).
		payload := dec.RemainingBytes()
		reply, err := ygsync.ApplySyncMessage(p.room.doc, payload, p)
		if err != nil {
			p.server.log().Debug("discarded unappliable sync message",
				"room", p.roomName, "err", err)
			return
		}
		if reply != nil {
			// Peer sent step-1 — send step-2 reply only to them.
			p.sendSync(reply)
		} else {
			// Peer sent step-2 or update — broadcast to all other peers.
			p.broadcastSync(payload)
		}

	case msgAwareness:
		// Awareness payload is VarBytes-wrapped (y-websocket protocol).
		awBytes, err := dec.ReadVarBytes()
		if err != nil {
			p.server.log().Debug("discarded malformed awareness frame",
				"room", p.roomName, "err", err)
			return
		}
		p.trackAwarenessClients(awBytes)
		if err := p.room.awareness.ApplyUpdate(awBytes, p); err != nil {
			// Drop invalid awareness updates; do not broadcast.
			p.server.log().Debug("discarded unappliable awareness update",
				"room", p.roomName, "err", err)
			return
		}
		p.broadcastAwareness(awBytes)

	case msgAuth:
		// Auth messages (type 2) are defined by y-websocket but not used by
		// this server. Silently ignore.

	case msgQueryAwareness:
		p.sendAwareness(p.room.awareness.EncodeUpdate(nil))

	case msgSyncReply:
		// Hocuspocus tag 4 (#55). Same payload shape as msgSync but the
		// sender explicitly does NOT want a reply — used by the original
		// requester to apply a SyncStep2 without bouncing another step-1
		// back, which would cause an infinite ping-pong on noisy links.
		// Apply locally and broadcast updates, but never reply with our
		// own step-1.
		payload := dec.RemainingBytes()
		if _, err := ygsync.ApplySyncMessage(p.room.doc, payload, p); err != nil {
			p.server.log().Debug("discarded unappliable sync(reply) message",
				"room", p.roomName, "err", err)
			return
		}
		p.broadcastSync(payload)

	case msgStateless:
		// Hocuspocus tag 5 (#55). Arbitrary out-of-band signal addressed
		// to the server only (no broadcast). Surface to the embedding
		// application via Server.OnStateless if configured.
		payload, err := dec.ReadVarString()
		if err != nil {
			p.server.log().Debug("discarded malformed stateless frame",
				"room", p.roomName, "err", err)
			return
		}
		if hook := p.server.OnStateless; hook != nil {
			p.server.safeHook("OnStateless", func() {
				hook(StatelessInfo{Room: p.roomName, Payload: payload, IsBroadcast: false})
			})
		}

	case msgBroadcastStateless:
		// Hocuspocus tag 6 (#55). Arbitrary out-of-band signal that the
		// sender wants delivered to all other peers in the room. Re-emit
		// as a plain Stateless (tag 5) frame so the receiving clients
		// can dispatch it through the same handler they already use for
		// server-originated stateless messages — matches Hocuspocus's
		// behaviour where BroadcastStateless from one connection arrives
		// at others as Stateless.
		payload, err := dec.ReadVarString()
		if err != nil {
			p.server.log().Debug("discarded malformed broadcast-stateless frame",
				"room", p.roomName, "err", err)
			return
		}
		p.broadcast(encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarUint(msgStateless)
			enc.WriteVarString(payload)
		}), true)
		if hook := p.server.OnStateless; hook != nil {
			p.server.safeHook("OnStateless", func() {
				hook(StatelessInfo{Room: p.roomName, Payload: payload, IsBroadcast: true})
			})
		}

	case msgClose:
		// Hocuspocus tag 7 (#55). Graceful close with an optional VarString
		// reason. The reason is informational; the canonical Hocuspocus
		// server discards it. We read it for the log line (best effort)
		// and close the underlying connection. handleDisconnect will run
		// when the read loop notices EOF.
		reason, _ := dec.ReadVarString() // optional; silent on parse error
		p.server.log().Info("peer requested close",
			"room", p.roomName, "reason", reason)
		_ = p.conn.Close()

	case msgSyncStatus:
		// Hocuspocus tag 8 (#55). Server→client ack carrying a single
		// VarUint flag (1 = applied, 0 = rejected). If a client sends it
		// to us, consume the payload silently — we don't track per-update
		// delivery confirmations.
		_, _ = dec.ReadVarUint()

	case msgPing:
		// Hocuspocus tag 9 (#55). Liveness check — reply with a single-byte
		// Pong frame. (gorilla/websocket's protocol-level ping/pong is
		// separate; Hocuspocus uses an application-level ping because some
		// load balancers eat the protocol frames.)
		p.write(encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarUint(msgPong)
		}))

	case msgPong:
		// Hocuspocus tag 10 (#55). Reply to a server-sent Ping. ygo does
		// not currently send Pings, so this is a no-op pass-through that
		// just keeps the dispatcher from dropping the frame.
	}
}

// trackAwarenessClients records which awareness clientIDs this peer owns
// so they can be removed when the peer disconnects.
func (p *peer) trackAwarenessClients(payload []byte) {
	dec := encoding.NewDecoder(payload)
	n, err := dec.ReadVarUint()
	if err != nil {
		return
	}
	p.cidMu.Lock()
	defer p.cidMu.Unlock()
	for i := uint64(0); i < n; i++ {
		clientID, err := dec.ReadVarUint()
		if err != nil {
			return
		}
		if _, err = dec.ReadVarUint(); err != nil { // clock
			return
		}
		if _, err = dec.ReadVarString(); err != nil { // state JSON
			return
		}
		// Cap the number of clientIDs a single peer may claim to prevent OOM
		// when handleDisconnect builds the removal slice (N-H4).
		if len(p.clientIDs) < maxAwarenessClientsPerPeer {
			p.clientIDs[clientID] = struct{}{}
		}
	}
}

// handleDisconnect removes the peer from the room and broadcasts awareness
// removal for all clientIDs the peer owned.
//
// disconnectOnce ensures the full teardown body runs at most once even when
// multiple callers race — e.g. broadcast()'s conn.Close() waking the read loop
// while the ctx-cancel goroutine concurrently reaches this point.
func (p *peer) handleDisconnect() {
	p.disconnectOnce.Do(func() {
		// H2: mark closed so concurrent broadcast writes skip this peer.
		// Close writeCh after marking closed so runWriter can drain and exit.
		// Both operations are done under wmu so broadcast() sees a consistent
		// state (closed=true is visible before writeCh is closed).
		p.wmu.Lock()
		p.closed = true
		close(p.writeCh)
		p.wmu.Unlock()

		// Wait for the per-peer writer goroutine to fully exit before we touch
		// the connection in the teardown path. The writer will see the closed
		// channel and exit cleanly.
		<-p.writerDone

		rm := p.room

		// Acquire both locks (server map first, then room) to atomically remove
		// the peer and, if the room is now empty, delete the room from the server
		// map and stop the persistence goroutine. This prevents a TOCTOU race
		// where a new peer joins between the emptiness check and room deletion,
		// which would fork the logical document into two rooms.
		p.server.rmu.Lock()
		rm.mu.Lock()
		delete(rm.peers, p)
		empty := len(rm.peers) == 0
		// roomEvicted distinguishes "we were the path that removed rm from
		// s.rooms" from "rm was already gone (CloseRoom won the race)".
		// Only the evicting path fires OnUnloadDocument — without this
		// guard a concurrent CloseRoom + last-peer-disconnect double-fires
		// the hook on the same room name. (#93 self-review B1.)
		roomEvicted := false
		if empty {
			if current, stillIn := p.server.rooms[p.roomName]; stillIn && current == rm {
				delete(p.server.rooms, p.roomName)
				roomEvicted = true
			}
			if rm.persistStop != nil {
				select {
				case <-rm.persistStop:
					// already closed by CloseRoom
				default:
					close(rm.persistStop)
				}
			}
		}
		rm.mu.Unlock()
		p.server.rmu.Unlock()

		// Release semaphore slots now that the peer has left.
		if rm.peerSem != nil {
			rm.peerSem.Release(1)
		}
		if sem := p.server.connSemaphore(); sem != nil {
			sem.Release(1)
		}

		// Wait for the persistence goroutine to drain buffered writes before the
		// room reference becomes garbage. This runs outside the locks above.
		if empty && rm.persistDone != nil {
			<-rm.persistDone
		}

		// #60 — Fire lifecycle hooks AFTER locks released and persistence
		// drain finished. OnLastPeer signals the 1→0 transition; the room
		// may or may not also be evicted (eviction is currently eager but
		// could become lazy in a future release). OnUnloadDocument fires
		// only when we were the path that actually evicted the room from
		// the server map (roomEvicted) — otherwise CloseRoom raced us and
		// has already / will already fire it. Both hooks are panic-safe.
		if empty {
			if hook := p.server.OnLastPeer; hook != nil {
				p.server.safeHook("OnLastPeer", func() {
					hook(context.Background(), p.roomName)
				})
			}
		}
		if roomEvicted {
			// Stop the awareness auto-expiry goroutine (if any) so it doesn't
			// outlive the room. Idempotent; no-op when expiry was never started.
			rm.awareness.Destroy()
			p.server.teardownRelayRoom(rm, p.roomName)
			if hook := p.server.OnUnloadDocument; hook != nil {
				p.server.safeHook("OnUnloadDocument", func() {
					hook(context.Background(), p.roomName)
				})
			}
		}

		p.cidMu.Lock()
		clientIDs := make([]uint64, 0, len(p.clientIDs))
		for id := range p.clientIDs {
			clientIDs = append(clientIDs, id)
		}
		p.cidMu.Unlock()

		if len(clientIDs) == 0 {
			return
		}

		removalBytes := encodeAwarenessRemoval(p.room.awareness, clientIDs)
		if removalBytes == nil {
			return
		}
		if err := p.room.awareness.ApplyUpdate(removalBytes, nil); err != nil {
			p.server.log().Warn("apply removal awareness failed", "room", p.roomName, "err", err)
		}
		p.broadcastAwarenessFromRoom(removalBytes)
	})
}

// encodeAwarenessRemoval builds a raw awareness update that marks the given
// client IDs as removed (null state, clock incremented by 1).
func encodeAwarenessRemoval(aw *awareness.Awareness, clientIDs []uint64) []byte {
	states := aw.GetStates()
	var toRemove []struct {
		id    uint64
		clock uint64
	}
	for _, id := range clientIDs {
		if cs, ok := states[id]; ok {
			toRemove = append(toRemove, struct {
				id    uint64
				clock uint64
			}{id, cs.Clock})
		}
	}
	if len(toRemove) == 0 {
		return nil
	}
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(len(toRemove)))
		for _, item := range toRemove {
			enc.WriteVarUint(item.id)
			enc.WriteVarUint(item.clock + 1)
			enc.WriteVarString("null")
		}
	})
}

// sendSync writes a sync message (outer type 0, raw payload) to this peer.
func (p *peer) sendSync(syncMsg []byte) {
	p.write(encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgSync)
		enc.WriteRaw(syncMsg) // sync payload is NOT VarBytes-wrapped
	}))
}

// sendAwareness writes an awareness message (outer type 1, VarBytes payload)
// to this peer.
func (p *peer) sendAwareness(awMsg []byte) {
	p.write(encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgAwareness)
		enc.WriteVarBytes(awMsg) // awareness payload IS VarBytes-wrapped
	}))
}

// broadcastSync sends a sync message to all OTHER peers in the room.
func (p *peer) broadcastSync(syncMsg []byte) {
	p.broadcast(encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgSync)
		enc.WriteRaw(syncMsg)
	}), true)
}

// broadcastAwareness sends an awareness message to all OTHER peers in the room.
func (p *peer) broadcastAwareness(awMsg []byte) {
	p.broadcast(encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgAwareness)
		enc.WriteVarBytes(awMsg)
	}), true)
}

// broadcastAwarenessFromRoom sends an awareness message to ALL peers (called
// from disconnect handler which has already removed itself from the room).
func (p *peer) broadcastAwarenessFromRoom(awMsg []byte) {
	p.broadcast(encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgAwareness)
		enc.WriteVarBytes(awMsg)
	}), false)
}

// broadcast enqueues data for delivery to peers in the room. If excludeSelf
// is true, the calling peer is excluded.
//
// When a target peer's writeCh is full (slow peer, dead connection, or
// receiver lagging), the peer is disconnected rather than dropping the
// message. This matches Rust yrs-warp's bounded-broadcast pattern: a
// dropped message would leave the peer with a silent gap in their sync
// stream that only resolves on the next exchange. Disconnecting forces
// a reconnect-and-resync flow which the CRDT's pending-structs
// machinery handles cleanly.
func (p *peer) broadcast(data []byte, excludeSelf bool) {
	p.room.mu.Lock()
	targets := make([]*peer, 0, len(p.room.peers))
	for other := range p.room.peers {
		if excludeSelf && other == p {
			continue
		}
		targets = append(targets, other)
	}
	p.room.mu.Unlock()

	for _, other := range targets {
		// Guard against sending to a closed channel: check p.closed under
		// wmu before attempting the channel send. handleDisconnect sets closed
		// under wmu before closing writeCh, so this is race-free.
		other.wmu.Lock()
		if other.closed {
			other.wmu.Unlock()
			continue
		}
		select {
		case other.writeCh <- data:
			// queued
		default:
			// Queue full — disconnect the slow peer.
			p.server.log().Warn("peer write queue full; closing slow peer",
				"room", other.roomName,
				"queueSize", cap(other.writeCh))
			_ = other.conn.Close()
			// The peer's read loop will detect the close and run
			// handleDisconnect to clean up room state.
		}
		other.wmu.Unlock()
	}
}

// write enqueues a raw binary message for delivery to this peer via the
// per-peer writer goroutine. H2: skips the write if the peer has already
// been marked closed. If the queue is full the peer is disconnected:
// a dropped handshake reply (sendSync / sendAwareness) leaves the remote
// peer hung waiting for a response it will never receive. Closing the
// connection forces a reconnect-and-resync, matching the broadcast contract.
func (p *peer) write(data []byte) {
	p.wmu.Lock()
	if p.closed {
		p.wmu.Unlock()
		return
	}
	select {
	case p.writeCh <- data:
	default:
		// Queue full during a direct send (e.g. handshake reply) — disconnect.
		// Unlike a silent drop, closing the connection lets the CRDT
		// pending-structs machinery recover via reconnect-and-resync.
		p.server.log().Warn("peer write queue full during direct send; closing peer",
			"room", p.roomName,
			"queueSize", cap(p.writeCh))
		_ = p.conn.Close()
	}
	p.wmu.Unlock()
}

// runWriter is the dedicated per-peer broadcast writer. It drains writeCh
// and serialises writes to the connection. Exits when writeCh is closed
// (during teardown) or when a write fails (the connection is then dead;
// the read loop will tear down the peer).
//
// This pattern mirrors Rust yrs-warp's per-peer sink task. It replaces
// the previous "spawn one goroutine per peer per broadcast" model, which
// produced unbounded goroutine churn under high broadcast cardinality
// and had no backpressure mechanism.
func (p *peer) runWriter() {
	defer close(p.writerDone)
	for data := range p.writeCh {
		p.wmu.Lock()
		if p.closed {
			p.wmu.Unlock()
			return
		}
		if err := p.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			p.server.log().Debug("set write deadline failed", "err", err)
			p.wmu.Unlock()
			return
		}
		if err := p.conn.WriteMessage(gws.BinaryMessage, data); err != nil {
			p.server.log().Warn("write to peer failed; closing", "room", p.roomName, "err", err)
			p.wmu.Unlock()
			return
		}
		p.wmu.Unlock()
	}
}
