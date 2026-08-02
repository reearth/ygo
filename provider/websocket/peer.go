package websocket

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
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
	// needsResync is set (under wmu) when a broadcast is dropped because writeCh
	// was full under SlowPeerResync. runWriter clears it and sends a full-state
	// resync once the queue drains, so the peer converges without a reconnect.
	needsResync       bool
	closeCode         int           // #104: WS close code queued via enqueueClose (0 = none)
	closeReason       string        // #104: WS close reason accompanying closeCode
	limiter           *rate.Limiter // per-peer inbound-message rate limiter; nil = unlimited (#51)
	readOnly          bool          // #59: drop this peer's inbound writes (sync step-2/update + awareness)
	hocuspocusFraming bool          // #104: docName-prefixed framing for this connection

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
	if p.hocuspocusFraming {
		docName, err := dec.ReadVarString()
		if err != nil {
			p.server.log().Debug("discarded malformed hocuspocus frame: unreadable docName",
				"room", p.roomName, "err", err)
			return
		}
		if docName != p.roomName {
			p.server.log().Debug("hocuspocus frame docName mismatch (processing anyway)",
				"room", p.roomName, "docName", docName)
		}
	}
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
		if p.readOnly {
			// Read-only peers may still request state (SyncStep1 → we reply with
			// SyncStep2), but must not push changes: drop SyncStep2/Update without
			// applying or broadcasting (#59). A malformed frame is dropped too.
			if subType, _, e := ygsync.ReadSyncMessage(payload); e != nil || subType != ygsync.MsgSyncStep1 {
				return
			}
		}
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
		if p.readOnly {
			return // #59: read-only peers' inbound awareness is dropped.
		}
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
		p.handleAuth(dec)

	case msgQueryAwareness:
		p.sendAwareness(p.room.awareness.EncodeUpdate(nil))

	case msgSyncReply:
		// Hocuspocus tag 4 (#55). Same payload shape as msgSync but the
		// sender explicitly does NOT want a reply — used by the original
		// requester to apply a SyncStep2 without bouncing another step-1
		// back, which would cause an infinite ping-pong on noisy links.
		// Apply locally and broadcast updates, but never reply with our
		// own step-1.
		if p.readOnly {
			return // #59: SyncReply carries a SyncStep2 write; drop it for read-only peers.
		}
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

// handleAuth processes a Hocuspocus in-band Auth (tag 2) sub-message. No-op
// when Server.OnTokenAuth is nil (backward compatible with the legacy
// silent-ignore behavior).
func (p *peer) handleAuth(dec *encoding.Decoder) {
	hook := p.server.OnTokenAuth
	if hook == nil {
		return
	}
	subType, err := dec.ReadVarUint()
	if err != nil || subType != authTypeToken {
		return // only client Token(0) sub-messages are actionable
	}
	token, err := dec.ReadVarString()
	if err != nil {
		p.server.log().Debug("discarded malformed auth token frame", "room", p.roomName, "err", err)
		return
	}

	cfg, authErr := p.safeTokenAuth(hook, token)
	if authErr != nil {
		p.write(encodeAuthMessage(authTypePermissionDenied, authErr.Error()))
		// Full error text goes in the PermissionDenied data frame above; the
		// WS close frame uses a short constant reason because control frames
		// cap the payload at 125 bytes (a long reason would drop the close).
		p.enqueueClose(wsCodeUnauthorized, wsReasonUnauthorized)
		return
	}

	p.readOnly = cfg.ReadOnly // safe: only read on this read-loop goroutine
	scope := "read-write"
	if cfg.ReadOnly {
		scope = "readonly"
	}
	p.write(encodeAuthMessage(authTypeAuthenticated, scope))
}

// safeTokenAuth calls the hook, converting a panic into a denial.
func (p *peer) safeTokenAuth(hook func(string, string) (ConnectionConfig, error), token string) (cfg ConnectionConfig, err error) {
	defer func() {
		if r := recover(); r != nil {
			p.server.log().Warn("OnTokenAuth panicked; denying", "room", p.roomName, "panic", r)
			cfg = ConnectionConfig{}
			err = fmt.Errorf("authentication error")
		}
	}()
	return hook(p.roomName, token)
}

// encodeAuthMessage builds a tag-2 auth reply: VarUint(msgAuth) VarUint(sub)
// VarString(s). docName framing (if enabled) is applied later in writeToConn.
//
// s is app-supplied (an OnTokenAuth hook's error text via authErr.Error(), or
// the "read-write"/"readonly" scope label) and this runs on a live connection
// goroutine. WriteVarString now panics on invalid UTF-8 (#209/Task 1), but an
// application returning a malformed error string is not this library's fault
// to crash a goroutine over, nor severe enough to warrant dropping the peer's
// connection the way a genuine protocol violation would. So this is the one
// place in the UTF-8 validation work that COERCES instead of rejecting:
// strings.ToValidUTF8 repairs the string in place (U+FFFD for bad runs),
// keeping the diagnostic readable and the connection alive. Do not "fix" this
// inconsistency with roomname.Valid's reject-outright rule — that guards a
// wire/relay identifier, this guards a live goroutine against an
// application's own string.
func encodeAuthMessage(subType uint64, s string) []byte {
	s = strings.ToValidUTF8(s, "�")
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(subType)
		enc.WriteVarString(s)
	})
}

// enqueueClose queues a WS close (with an application close code) AFTER any
// frames already in writeCh. The close control frame is emitted by runWriter
// on the writer goroutine, preserving ordering (a preceding PermissionDenied
// data frame is flushed first) and the single-writer invariant. A nil sentinel
// on writeCh signals runWriter to close.
//
// Callers MUST invoke enqueueClose only from the peer's own read-loop
// goroutine (the same goroutine that runs the deferred handleDisconnect).
// Unlike write()/broadcast(), which hold wmu across both the closed-check and
// the writeCh send, enqueueClose releases wmu before the send below: its
// full-queue fallback calls sendCloseFrame(), which re-locks wmu, and
// sync.Mutex is not reentrant, so holding wmu across the send would deadlock
// on that path. Because handleDisconnect (the sole close(p.writeCh) site)
// runs sequentially after enqueueClose's caller on the same goroutine, this
// send can never race the channel close today.
func (p *peer) enqueueClose(code int, reason string) {
	p.wmu.Lock()
	if p.closed {
		p.wmu.Unlock()
		return
	}
	p.closeCode = code
	p.closeReason = reason
	p.wmu.Unlock()

	func() {
		defer func() {
			if recover() != nil {
				// writeCh already closed by handleDisconnect (a future
				// cross-goroutine misuse) — close directly instead of
				// panicking.
				p.sendCloseFrame()
			}
		}()
		select {
		case p.writeCh <- nil:
		default:
			// Queue full: best-effort direct close (ordering already at risk).
			p.sendCloseFrame()
		}
	}()
}

// sendCloseFrame writes the queued WS close control frame then closes the
// connection. Called only from the writer goroutine (via runWriter) or the
// enqueueClose full-queue fallback.
func (p *peer) sendCloseFrame() {
	p.wmu.Lock()
	code, reason := p.closeCode, p.closeReason
	p.wmu.Unlock()
	if code != 0 {
		_ = p.conn.WriteControl(gws.CloseMessage,
			gws.FormatCloseMessage(code, reason),
			time.Now().Add(writeTimeout))
	}
	_ = p.conn.Close()
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
		rm.mu.Unlock()
		p.server.rmu.Unlock()

		// Release semaphore slots now that the peer has left. These run
		// regardless of the eviction decision below.
		if rm.peerSem != nil {
			rm.peerSem.Release(1)
		}
		if sem := p.server.connSemaphore(); sem != nil {
			sem.Release(1)
		}

		// roomEvicted distinguishes "we were the path that removed rm from
		// s.rooms" from "rm was already gone (CloseRoom won the race)".
		// Only the evicting path fires OnUnloadDocument — without this
		// guard a concurrent CloseRoom + last-peer-disconnect double-fires
		// the hook on the same room name. (#93 self-review B1.)
		roomEvicted := false

		// FLUSH-BEFORE-EVICT (#175 follow-up). When the room is now empty, make
		// the pending coalesced batch durable BEFORE evicting the room from
		// s.rooms — while the worker is still alive and the room is still
		// discoverable. A quick reconnect then either finds this warm room or
		// reads an up-to-date store, never a stale one. We must NOT close
		// persistStop for this flush: closing it triggers the worker's own
		// exit-flush, but a reconnect can beat that; the flushReq path is what
		// guarantees durability-while-discoverable. Guard against a worker that
		// already exited (e.g. CloseRoom raced us) via the persistDone branch.
		//
		// The ack is a real durability barrier: flushOK is true only if the batch
		// was actually persisted. On failure we ABORT eviction below — keeping the
		// room + worker alive so the retained batch is retried on the next teardown
		// / Shutdown, and a reconnect meanwhile finds the warm in-memory doc rather
		// than reloading a stale store (which would silently lose the edit).
		// CRITICAL: no lock (rmu / rm.mu) may be held across the send/ack; the ack
		// is buffered (cap 1) so the worker never blocks reporting the result.
		flushOK := true
		if empty && rm.flushReq != nil {
			ack := make(chan bool, 1)
			select {
			case rm.flushReq <- ack:
				flushOK = <-ack
			case <-rm.persistDone:
				// Worker already gone (raced by CloseRoom/shutdown); its exit-flush
				// handled durability best-effort. Leave flushOK true so eviction can
				// still finish tearing down this defunct room.
			}
		}

		// Re-check emptiness under the locks and evict only if still empty AND the
		// flush persisted: a peer may have reconnected during the (possibly slow)
		// flush above, or the flush may have failed. evict gates both the
		// persistStop close and the persistDone wait so they stay consistent —
		// when we abort (flush failed) the worker stays alive and we must NOT wait
		// on persistDone (it would block forever).
		//
		// IDLE RESIDENCY (#183). When Server.RoomIdleTimeout > 0, a still-empty
		// room whose flush succeeded is NOT evicted: instead we stamp
		// rm.idleSince and leave it resident (persistStop stays open, worker
		// stays alive) so a rejoin reuses the warm in-memory doc with no
		// reload. This only changes what happens on the safe (flushOK) path;
		// a flush failure still leaves the room alive exactly as before
		// (evict stays false), matching pre-#183 behaviour, and does NOT stamp
		// idleSince — an unflushed room isn't safely idle. RoomIdleTimeout==0
		// takes the exact pre-#183 branch (evict = stillEmpty && flushOK), so
		// behaviour for existing callers is byte-identical.
		stillEmpty := false
		evict := false
		if empty {
			idleTimeout := p.server.RoomIdleTimeout
			p.server.rmu.Lock()
			rm.mu.Lock()
			stillEmpty = len(rm.peers) == 0
			if stillEmpty && flushOK {
				if idleTimeout > 0 {
					rm.idleSince = time.Now()
				} else {
					evict = true
				}
			}
			if evict {
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

			// Wait for the persistence goroutine to drain and exit before the
			// room reference becomes garbage. Only wait when we actually evicted
			// (closed persistStop, or found it already closed by CloseRoom):
			// otherwise the worker is still running (a peer rejoined, or we aborted
			// on flush failure) and this would block forever.
			if evict && rm.persistDone != nil {
				<-rm.persistDone
			}
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
			// Queue full.
			if p.server.SlowPeerPolicy == SlowPeerResync {
				// Drop this stale delta and flag an in-place resync; runWriter
				// sends a full-state SyncStep2 once the queue drains, so the peer
				// converges without a reconnect.
				other.needsResync = true
				p.server.log().Debug("peer write queue full; scheduling in-place resync",
					"room", other.roomName,
					"queueSize", cap(other.writeCh))
			} else {
				// Disconnect the slow peer; the read loop runs handleDisconnect.
				p.server.log().Warn("peer write queue full; closing slow peer",
					"room", other.roomName,
					"queueSize", cap(other.writeCh))
				_ = other.conn.Close()
			}
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
		if data == nil {
			// close-directive sentinel (#104 G1): flush is done; emit the WS
			// close control frame on this goroutine, then stop.
			p.sendCloseFrame()
			return
		}
		if !p.writeToConn(data) {
			return
		}

		// SlowPeerResync: if a broadcast was dropped while this peer was behind,
		// resync it now that the backlog has drained. Gated on the policy so the
		// default SlowPeerDisconnect path takes no extra lock per write.
		// SlowPeerPolicy, like the other Server config fields, is expected to be
		// set before ServeHTTP and not mutated while serving, so this read is
		// unsynchronised.
		if p.server.SlowPeerPolicy == SlowPeerResync && !p.maybeResync() {
			return
		}
	}
}

// writeToConn performs one conn write for the runWriter goroutine. wmu is held
// only long enough to read the closed flag; the (possibly blocking) WriteMessage
// runs WITHOUT wmu so that a slow peer cannot stall broadcast(), which must take
// the same wmu to enqueue frames for OTHER peers. This is safe because runWriter
// is the sole goroutine that ever calls conn.WriteMessage (all sends funnel
// through writeCh), and gorilla/websocket permits conn.Close() — used by the
// disconnect path and teardown — to run concurrently with a write. Returns false
// when the writer goroutine should exit.
func (p *peer) writeToConn(data []byte) bool {
	p.wmu.Lock()
	closed := p.closed
	p.wmu.Unlock()
	if closed {
		return false
	}
	if p.hocuspocusFraming {
		data = encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarString(p.roomName)
			enc.WriteRaw(data)
		})
	}
	if err := p.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		p.server.log().Debug("set write deadline failed", "err", err)
		return false
	}
	if err := p.conn.WriteMessage(gws.BinaryMessage, data); err != nil {
		p.server.log().Warn("write to peer failed; closing", "room", p.roomName, "err", err)
		return false
	}
	return true
}

// maybeResync sends a one-shot full-state resync (SyncStep2 + current awareness)
// when a broadcast was dropped under SlowPeerResync and the write queue has since
// drained. The full state supersedes the dropped incremental updates, so the peer
// converges without a reconnect. Returns false if the connection is dead and the
// writer goroutine should exit.
//
// This runs only in the runWriter goroutine, so the frame writes here cannot race
// another conn writer; wmu is taken only to read the closed/needsResync flags,
// never held across a write (see writeToConn for why).
func (p *peer) maybeResync() bool {
	p.wmu.Lock()
	// Wait until the backlog has fully drained so the resync is not immediately
	// followed by now-stale queued deltas.
	if p.closed || !p.needsResync || len(p.writeCh) > 0 {
		notClosed := !p.closed
		p.wmu.Unlock()
		return notClosed
	}
	p.needsResync = false
	p.wmu.Unlock()

	// Build resync frames without holding wmu: crdt.Doc and Awareness are
	// internally synchronised (mirrors the initial-sync path).
	step2 := encodeSyncStep2Msg(crdt.EncodeStateAsUpdateV1(p.room.doc, nil))
	syncFrame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgSync)
		enc.WriteRaw(step2)
	})
	awFrame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgAwareness)
		enc.WriteVarBytes(p.room.awareness.EncodeUpdate(nil))
	})

	// Send the frames via the same wmu-free write path as normal broadcasts, so a
	// slow peer cannot stall broadcast() during the resync either.
	for _, frame := range [][]byte{syncFrame, awFrame} {
		if !p.writeToConn(frame) {
			return false
		}
	}
	p.server.log().Debug("sent in-place resync to recovered slow peer", "room", p.roomName)
	return true
}
