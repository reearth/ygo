package websocket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	inputClosed       atomic.Bool   // a close is queued or sent; the read loop drops input from then on

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
		subType, update, err := ygsync.ReadSyncMessage(payload)
		if err != nil {
			p.discardMalformedSync(err)
			return
		}
		if subType != ygsync.MsgSyncStep1 {
			p.applySyncUpdate(subType, update, payload)
			return
		}
		// Peer sent step-1 — send step-2 reply only to them. Read-only peers
		// may still request state (#59).
		reply, err := ygsync.ApplySyncMessage(p.room.doc, payload, p)
		if err != nil {
			p.server.log().Debug("discarded unappliable sync message",
				"room", p.roomName, "err", err)
			return
		}
		p.sendSync(reply)

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
		if err := p.trackAwarenessClients(awBytes); err != nil {
			p.server.log().Debug("discarded awareness frame at the admission gate",
				"room", p.roomName, "reason", err)
			return
		}
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
		payload := dec.RemainingBytes()
		subType, update, err := ygsync.ReadSyncMessage(payload)
		if err != nil {
			p.discardMalformedSync(err)
			return
		}
		if subType != ygsync.MsgSyncStep1 {
			p.applySyncUpdate(subType, update, payload)
			return
		}
		if p.readOnly {
			return // #59: read-only peers' SyncReply frames are dropped.
		}
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
		// VarUint flag (1 = applied, 0 = not applied), which applySyncUpdate
		// sends for every SyncStep2 or Update. If a client sends one to us,
		// consume the payload silently.
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

// applySyncUpdate handles a SyncStep2 or Update (update, the sync payload's
// content) that a peer sent under Sync (tag 0) or SyncReply (tag 4): it applies
// the update to the room, through Server.OnPeerUpdate when one is set, and
// broadcasts payload to the room's other peers once the room has applied it. A
// read-only peer's update is neither applied nor broadcast (#59). An update the
// hook refused is not broadcast, and the peer is then closed (see
// PeerUpdateHook).
//
// On a connection using Hocuspocus framing it answers every such frame with
// exactly one SyncStatus (tag 8), in the order the frames arrived, so a client
// can pair each answer with the frame it sent: 1 once the room applied the
// update; for a read-only peer, 0 for an Update, and for a SyncStep2 1 when the
// room already holds everything in it and 0 otherwise, which is how
// @hocuspocus/server answers when the update decodes. Two differences: (1) an
// update the room does not fully apply (ApplyUpdateV1 fails: its V1 bytes do
// not decode, or it overflows MaxPendingItems) is answered 0, where
// @hocuspocus/server answers 1, since Yjs has no pending cap and y-protocols
// swallows the decode error; 1 would tell the client the room holds an edit it
// does not. Whatever part of such an update did integrate is still broadcast,
// on either framing. (2) A read-only peer's SyncStep2 whose update does not
// decode is answered 0, where @hocuspocus/server closes with 4403, which would
// stop @hocuspocus/provider from reconnecting. A sync message whose own framing
// does not decode never reaches here: see discardMalformedSync.
func (p *peer) applySyncUpdate(subType int, update, payload []byte) {
	if p.readOnly {
		if p.hocuspocusFraming {
			p.sendSyncStatus(subType == ygsync.MsgSyncStep2 && p.roomHolds(update))
		}
		return
	}
	applied, closeWith := p.applyPeerUpdate(update)
	if !applied {
		p.sendSyncStatus(false)
		if closeWith != nil {
			p.enqueueClose(closeWith.Code, closeWith.Reason)
		}
		return
	}
	p.broadcastSync(payload)
	p.sendSyncStatus(true)
}

// applyToRoom applies update to the room's document. ApplyUpdateV1 integrates
// item by item, so an update that overflows MaxPendingItems can leave part of
// itself in the room; when it fails, applyToRoom broadcasts that part, and only
// that part, to the room's other peers.
func (p *peer) applyToRoom(update []byte) error {
	landed, err := crdt.ApplyUpdateV1Landed(p.room.doc, update, p)
	if err != nil && landed != nil {
		p.broadcastSync(ygsync.EncodeUpdate(landed))
	}
	return err
}

// roomHolds reports whether the room's document already holds everything the
// V1 update carries. An update that does not decode is not held.
func (p *peer) roomHolds(update []byte) bool {
	held, err := crdt.DocContainsUpdateV1(p.room.doc, update)
	if err != nil {
		p.server.log().Debug("read-only sync message does not decode",
			"room", p.roomName, "err", err)
		return false
	}
	return held
}

// sendSyncStatus writes a SyncStatus (tag 8) frame carrying 1 when applied is
// true and 0 otherwise, on a connection using Hocuspocus framing only:
// y-websocket clients do not know tag 8.
func (p *peer) sendSyncStatus(applied bool) {
	if !p.hocuspocusFraming {
		return
	}
	var flag uint64
	if applied {
		flag = 1
	}
	p.write(encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(msgSyncStatus)
		enc.WriteVarUint(flag)
	}))
}

// discardMalformedSync drops a Sync or SyncReply frame whose sync message
// does not decode. On a connection using Hocuspocus framing it also closes
// the connection with 1002 (protocol error) and sends no SyncStatus: the frame
// can be paired with no answer, so the client's pairing ends with the
// connection. @hocuspocus/server 2.15.3 differs: on a read-write connection it
// answers a truncated SyncStep2 or Update with SyncStatus(1), because
// y-protocols catches the read error along with the apply error, and it
// answers 0 or closes with 4403 for the other frames it cannot read. An answer
// of 1 would acknowledge an edit the room never saw, and 4403 would stop
// @hocuspocus/provider from reconnecting.
func (p *peer) discardMalformedSync(err error) {
	p.server.log().Debug("discarded malformed sync message",
		"room", p.roomName, "err", err)
	if p.hocuspocusFraming {
		p.enqueueClose(gws.CloseProtocolError, "malformed sync message")
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
// on writeCh signals runWriter to close. From the call on, the read loop
// handles no further frame from this peer, including ones it has already
// received: the close means nothing more from the connection lands.
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
	p.inputClosed.Store(true)
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

// awarenessEntry is one decoded entry of an awareness frame: the client it
// speaks for, the clock it speaks at, and whether it retires that client
// rather than asserting state.
type awarenessEntry struct {
	clientID uint64
	clock    uint64
	removal  bool
}

// Reasons an awareness frame is refused before it reaches the room. The first
// three are frames Awareness.ApplyUpdate would refuse wholesale; the gate must
// refuse exactly those too, or it would release slots for removals the room
// never applies.
var (
	errAwarenessTruncated     = errors.New("truncated awareness frame")
	errAwarenessTooManyClient = errors.New("awareness frame exceeds the maximum client count")
	errAwarenessStateTooLarge = errors.New("awareness client state exceeds the maximum size")
	errAwarenessConnCap       = errors.New("connection exceeds its awareness client cap")
)

// decodeAwarenessEntries reads a frame's entries in wire order. It mirrors
// Awareness.applyUpdate's decode — same reads, same limits, same null test —
// so the gate judges exactly the entries the apply would see. On error the
// entries read before that point are still returned, since the ungated path
// tracks them as it always has.
func decodeAwarenessEntries(payload []byte) (entries []awarenessEntry, err error) {
	dec := encoding.NewDecoder(payload)
	n, err := dec.ReadVarUint()
	if err != nil {
		return nil, errAwarenessTruncated
	}
	if n > maxAwarenessEntriesPerFrame {
		return nil, errAwarenessTooManyClient
	}
	entries = make([]awarenessEntry, 0, n)
	for i := uint64(0); i < n; i++ {
		clientID, err := dec.ReadVarUint()
		if err != nil {
			return entries, errAwarenessTruncated
		}
		clock, err := dec.ReadVarUint()
		if err != nil {
			return entries, errAwarenessTruncated
		}
		state, err := dec.ReadVarBytes()
		if err != nil {
			return entries, errAwarenessTruncated
		}
		if len(state) > maxAwarenessStateBytesPerEntry {
			return entries, errAwarenessStateTooLarge
		}
		entries = append(entries, awarenessEntry{
			clientID: clientID,
			clock:    clock,
			removal:  len(state) == 0 || string(state) == "null",
		})
	}
	return entries, nil
}

// trackAwarenessClients records which awareness clientIDs this peer owns, so
// they can be removed when it disconnects, and reports whether the frame may
// be applied — nil to apply it, otherwise why it is refused. An ID counts
// against Server.MaxAwarenessClientsPerConn while this peer asserts state the
// room did not already hold live for it; a frame that would push the peer past
// that cap is rejected whole, so a presence flood is contained at the
// connection that sends it.
func (p *peer) trackAwarenessClients(payload []byte) error {
	entries, err := decodeAwarenessEntries(payload)
	limit := p.server.maxAwarenessClientsPerConn()
	if limit < 0 {
		// Gate disabled: track every ID claimed, up to the ceiling that bounds
		// the removal slice handleDisconnect builds (N-H4).
		p.cidMu.Lock()
		defer p.cidMu.Unlock()
		for _, e := range entries {
			if len(p.clientIDs) < maxAwarenessClientsPerPeer {
				p.clientIDs[e.clientID] = struct{}{}
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !p.admitAwarenessEntries(entries, limit) {
		return errAwarenessConnCap
	}
	return nil
}

// ownershipUndo holds the pre-frame ownership of each ID a frame's walk
// changed, so a rejected frame can be undone exactly whatever order it touched
// them in.
type ownershipUndo map[uint64]bool

// record notes id's ownership the first time the walk changes it.
func (u *ownershipUndo) record(id uint64, owned bool) {
	if *u == nil {
		*u = make(ownershipUndo, 4)
	}
	if _, seen := (*u)[id]; !seen {
		(*u)[id] = owned
	}
}

// restore puts every recorded ID back the way the frame found it.
func (u ownershipUndo) restore(set map[uint64]struct{}) {
	for id, owned := range u {
		if owned {
			set[id] = struct{}{}
		} else {
			delete(set, id)
		}
	}
}

// roomView is what the room shows for one client ID, advanced entry by entry
// as the walk models what the frame will do to it.
type roomView struct {
	known bool
	clock uint64
	live  bool
}

// honours reports whether Awareness.applyUpdate will take this entry, mirroring
// its clock gate: a strictly-older entry is discarded, and one at the same
// clock counts only when it retires a live client. A null for the awareness's
// own client is never honoured — the applier protects its local state instead
// of removing it.
func (v roomView) honours(e awarenessEntry, localID uint64) bool {
	if e.removal && e.clientID == localID {
		return false
	}
	if !v.known || e.clock > v.clock {
		return true
	}
	return e.clock == v.clock && e.removal && v.live
}

// admitAwarenessEntries applies a decoded frame to this peer's owned-ID set and
// reports whether the frame fits its budget. The whole frame is judged before
// the verdict, so a removal early in it frees a slot for an introduction later
// in it, and a rejected frame leaves the set as it found it.
//
// An entry the applier will discard changes neither the room nor the budget,
// so the walk skips it — with one deliberate exception for removals, below.
// That matters for removals above all: a slot freed for a null the applier
// throws away would leave the ID live in the room and owned by nobody, which
// is the whole cap bypassed one stale null at a time.
//
// The room is read before cidMu is taken: ClientView takes the awareness lock,
// and the two must never nest.
func (p *peer) admitAwarenessEntries(entries []awarenessEntry, limit int) bool {
	localID := p.room.awareness.ClientID()
	view := make(map[uint64]roomView, len(entries))
	for _, e := range entries {
		if _, done := view[e.clientID]; done {
			continue
		}
		clock, known, live := p.room.awareness.ClientView(e.clientID)
		view[e.clientID] = roomView{known: known, clock: clock, live: live}
	}

	p.cidMu.Lock()
	defer p.cidMu.Unlock()
	var undo ownershipUndo
	for _, e := range entries {
		v := view[e.clientID]
		honoured := v.honours(e, localID)
		_, owned := p.clientIDs[e.clientID]
		switch {
		case e.removal:
			// A slot is also released for an ID that is not live, even when the
			// applier discards the null: holding one for a dead client can never
			// leak presence, and re-asserting it is charged afresh. Expiry
			// tombstones at the unchanged clock, so a client's own null for an
			// expired ID is exactly that case.
			if !honoured && v.live {
				continue
			}
			if owned {
				undo.record(e.clientID, true)
				delete(p.clientIDs, e.clientID)
			}
		case !honoured:
			continue
		case owned || v.live:
			// Re-asserting a client the peer already speaks for, or one the room
			// already holds, introduces nothing.
		case len(p.clientIDs) >= limit:
			undo.restore(p.clientIDs)
			return false
		default:
			undo.record(e.clientID, false)
			p.clientIDs[e.clientID] = struct{}{}
		}
		// Later entries see what this one leaves behind, so a frame cannot
		// retire an ID and then re-assert it for free, nor retire one it has
		// just asserted at a newer clock.
		if honoured {
			view[e.clientID] = roomView{known: true, clock: e.clock, live: !e.removal}
		}
	}
	return true
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
// client IDs as removed (null state), at each client's CURRENT clock as the
// room's shared Awareness sees it — deliberately NOT incremented (#226).
//
// An earlier version bumped by one, on the (wrong) assumption that a
// removal needs a fresh clock to be admitted. It does not: Awareness.
// ApplyUpdate's equal-clock rule already accepts a null entry at the SAME
// clock as an existing active one ("equal clock + null + currently-active
// -> accept" — see that method's doc and its #73 vector C2 comment) — every
// id reaching this function came from aw.GetStates(), which only returns
// active entries, so that gate is satisfied as-is. Bumping past it instead
// created a real bug: a disconnecting client's rejoin heartbeat (Awareness.
// Heartbeat) ALSO bumps by exactly one, from that client's own last-known
// clock — which, absent any intervening update, is the identical base this
// function reads from aw.GetStates(). The removal and the rejoin then
// landed on the SAME clock, and at an equal clock ApplyUpdate always favors
// the null side over an active one no matter which arrives first at a
// given peer — so a client that disconnected and immediately rejoined had
// its genuine reappearance silently suppressed everywhere else in the room.
// Leaving the clock unbumped removes the tie entirely: the rejoin's own
// bump is now strictly newer than the removal in every case, and
// ApplyUpdate's ordinary newer-clock path (not the equal-clock one) admits
// it unconditionally, regardless of arrival order. See
// TestUnit_EncodeAwarenessRemoval_UnbumpedSurvivesRejoinTie.
//
// This also matches y-protocols' removeAwarenessStates: it bumps the clock
// only when the removed client IS the awareness instance's own local
// client (a self-transition, mirrored here by Awareness.ApplyUpdate's
// separate self-state-protection path); for every other client — exactly
// this function's case, synthesising a removal on ANOTHER client's behalf —
// it just deletes the entry and leaves the clock alone.
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
			enc.WriteVarUint(item.clock)
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
