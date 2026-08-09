package client

import (
	"context"
	"fmt"
	"net/http"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// Timeouts the dial loop applies per connection. Both mirror
// provider/websocket's server-side equivalents (its writeTimeout const and
// handshakeTimeout default) so a client and ygo's own server give up on a
// stuck peer on the same timescale rather than one side hanging on an
// already-abandoned socket.
//
// These are deliberately NOT Options fields: nothing in #165 needs them
// tunable, and every knob added to Options is one more thing an embedder can
// get wrong. Promote them only when a real deployment needs it.
const (
	dialHandshakeTimeout = 10 * time.Second
	writeTimeout         = 10 * time.Second
)

// reconnectBackoffBase is the Full Jitter base duration runReconnectLoop
// constructs its backoff with. Unlike MaxBackoff, this is not an Options
// field: #165 has no requirement to tune the SHORT end of the schedule, and
// letting only the ceiling be configurable is one fewer number an embedder
// can get wrong for no real benefit.
const reconnectBackoffBase = 500 * time.Millisecond

// readDeadlineMultiplier is how many Options.PingInterval periods of total
// silence (no pong, no data frame — see runLoop's keepalive section) this
// client tolerates before treating a connection as dead. 2x means a single
// missed pong (one dropped packet, one slow GC pause, one busy event loop on
// either side) is not by itself fatal — the NEXT ping still has a full
// interval to succeed before the deadline expires — while two consecutive
// misses fires well before an embedding application would notice anything
// wrong on its own. This mirrors y-websocket's own ping-every-30s,
// terminate-at-2x convention (#165).
const readDeadlineMultiplier = 2

// session is one connection's worth of loop state: the socket, and whatever
// must be remembered for the lifetime of THAT connection rather than the
// lifetime of the Client (currently just whether the handshake has completed).
// Splitting it out from Client is what lets runReconnectLoop start a fresh
// session per attempt without having to remember which Client fields need
// resetting — a new connection is a new session, by construction.
//
// # Single-writer invariant
//
// The precise rule, because a looser statement of it would be false and the
// difference matters: **the loop goroutine is the only writer of DATA frames**
// (WriteMessage). Control frames may additionally be emitted by the read
// goroutine, and MUST only ever be emitted via conn.WriteControl.
//
// That second clause is not a concession this file makes — gorilla does it
// unprompted, from inside ReadMessage. Its default ping handler answers with a
// pong (conn.go:1161), its default close handler echoes a close frame
// (conn.go:984), and exceeding the read limit sends CloseMessageTooBig
// (conn.go:925). All three run on whichever goroutine is in ReadMessage, i.e.
// this file's read pump. It is safe because WriteControl is the one write
// method gorilla documents as callable concurrently with every other method;
// it takes its own mutex and does not touch the shared write buffer that
// WriteMessage owns.
//
// So: anything added to the read side later must use WriteControl and nothing
// else. In particular, an application-level keepalive built the obvious way —
// SetWriteDeadline followed by WriteMessage, from the read goroutine or a
// timer goroutine — would corrupt the connection by interleaving with a data
// frame the loop is part-way through writing, and would do so intermittently
// and under load rather than in a test. A keepalive belongs either in the
// loop's own select, or as conn.WriteControl(PingMessage, …).
//
// Every method on session that writes a DATA frame (write, and everything
// that calls it) runs on the loop goroutine that runLoop owns. That is not a
// coincidence to preserve case-by-case; it is what this whole file is
// arranged around. session.ping (added for #165 Task 7's keepalive) is the
// one method worth naming as an exception to that sentence rather than a
// silent instance of it: it writes a CONTROL frame via WriteControl, which
// gorilla documents as safe to call from ANY goroutine, concurrently with a
// WriteMessage the loop is part-way through — so nothing would actually
// break if ping ran elsewhere. It runs on the loop goroutine anyway, from a
// ticker case alongside every other select case here, purely so this file
// keeps one simple, auditable story ("every write this session performs
// happens on the loop goroutine") instead of a data/control split that would
// be equally correct but harder to verify at a glance. The other half of the
// arrangement is Client.onDocUpdate: a write that blocks on a slow socket
// must never be reachable from a caller's Transact, so the observer hands
// off to a lane instead of writing.
type session struct {
	c    *Client
	conn *gws.Conn
	// synced records that this connection has applied a SyncStep2, so a
	// server that later sends another one (a resync, e.g. provider/
	// websocket's SlowPeerResync path) does not re-emit StateSynced as
	// though a new handshake had completed, and does not call onSynced a
	// second time for the same connection.
	synced bool
	// onSynced, if non-nil, is called exactly once per session — the first
	// time this connection's handshake completes — by handleFrame, right
	// alongside c.markSynced(). runReconnectLoop passes its backoff's
	// reset method here, which is what makes "reset after a successful
	// HANDSHAKE, not merely a successful dial" (see backoff.reset's doc)
	// actually happen at the one moment that is true.
	onSynced func()
}

// runLoop performs one full connection lifecycle: dial, y-websocket
// handshake, then live bidirectional sync until ctx is cancelled or the
// connection fails. It returns nil for a clean stop (ctx cancelled) and an
// error describing what went wrong otherwise.
//
// onSynced, if non-nil, is called once this connection's handshake
// completes (see session.onSynced). runLoop itself has no opinion about
// what that callback does — runReconnectLoop is the only caller, and it
// passes its backoff's reset method — but it is the thing that has to
// thread the callback down to where the handshake's completion is actually
// detected, which is inside handleFrame, not here.
//
// runLoop never retries anything itself: reconnect and backoff live one
// layer up, in runReconnectLoop, which is what lets THIS function stay
// "one connection, start to finish" — the shape a reader auditing the
// single-writer invariant below wants, rather than one that also has to
// reason about a retry loop wrapped around it.
//
// # Why the reads happen on a separate goroutine
//
// gorilla/websocket's ReadMessage blocks, and the loop must simultaneously be
// able to notice outbound work appearing on the lane. So a read pump goroutine
// does nothing but ReadMessage and hand frames to the loop over a channel. It
// never touches the Doc or the lane, and it never writes a data frame — though
// gorilla itself may emit control frames from inside its ReadMessage call, via
// the concurrency-safe WriteControl path; see session's doc for the exact
// invariant and for what that forbids a later change from adding here.
//
// # Keepalive (#165 Task 7): ping from the loop, deadline refreshed on the read pump
//
// A half-open connection — the peer vanished without ever sending a FIN or
// RST — never produces a read error on its own; ReadMessage just blocks
// forever, and the reconnect loop above this function never gets a chance to
// run. This is solved with the two gorilla mechanisms designed for exactly
// this, kept on the goroutines their own concurrency contracts require:
//
//   - A ping every Options.PingInterval, sent via session.ping (WriteControl,
//     never write/WriteMessage) from THIS function's own select loop below,
//     alongside a ticker case — not from a separate timer goroutine, and not
//     via SetWriteDeadline+WriteMessage from anywhere, which is precisely the
//     naive keepalive session's doc warns would corrupt the connection by
//     racing the loop's own data writes.
//   - A read deadline of readDeadlineMultiplier * Options.PingInterval, set
//     once below before the read pump starts, and refreshed from two places
//     that both run on the read pump goroutine: the pong handler (for the
//     pong-only case, which never surfaces as a ReadMessage return — see
//     gorilla's SetPongHandler doc) and the pump's own loop body after every
//     successfully read frame (so ordinary sync traffic counts as liveness
//     too, not only pongs — a busy connection that happens to need no pings
//     must not be killed by one anyway). Refreshing the read deadline is a
//     read-side call (conn.SetReadDeadline forwards straight to the
//     underlying net.Conn, which is documented safe for concurrent use with
//     unrelated reads AND writes on other goroutines — unlike
//     conn.SetWriteDeadline, which is an unguarded field gorilla's own
//     WriteMessage reads, see gorilla's conn.go), so doing it from the read
//     pump while the loop goroutine is mid-write is not the hazard
//     SetWriteDeadline from the wrong goroutine would be.
//
// A missed pong therefore does not need its own detection path: the read
// deadline simply expires, ReadMessage returns an error, that error reaches
// readErr below exactly like any other read failure, and runReconnectLoop
// takes over from there. Keepalive's entire job is converting silence into
// that ordinary error; it must not — and does not — grow a second reconnect
// mechanism of its own.
func (c *Client) runLoop(ctx context.Context, onSynced func()) error {
	c.emitStatus(Status{State: StateConnecting})

	dialer := &gws.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: dialHandshakeTimeout,
	}
	// c.opts.Header was defensively copied by New, so a caller mutating their
	// own map after construction cannot change what this dial sends.
	conn, resp, err := dialer.DialContext(ctx, c.opts.URL, c.opts.Header)
	if resp != nil && resp.Body != nil {
		// gorilla hands back the HTTP response on a rejected upgrade (e.g. a
		// 401 from provider/websocket's Authorize hook). Nothing here reads
		// it, but leaving it open would leak the connection out of the
		// transport's pool.
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("client: dial %q: %w", c.opts.URL, err)
	}
	conn.SetReadLimit(c.opts.ReadLimit)

	// pingInterval and readDeadline are captured once per connection rather
	// than re-read from c.opts wherever they are needed below: Options is
	// immutable after New returns (see New's doc), so there is nothing to
	// re-read, and a local keeps both the ticker below and the read-deadline
	// math here and in the read pump simple and obviously consistent with
	// each other.
	pingInterval := c.opts.PingInterval
	readDeadline := readDeadlineMultiplier * pingInterval

	// Install the initial read deadline and the pong handler that renews it
	// BEFORE the read pump goroutine below ever calls ReadMessage — there is
	// no concurrent reader yet at this point, so setting both here needs no
	// synchronisation of its own. See runLoop's "Keepalive" doc section above
	// for why this call is safe to make again later from the read pump
	// goroutine while the loop goroutine is concurrently writing.
	if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
		return fmt.Errorf("client: set initial read deadline: %w", err)
	}
	// A pong is the one case that does NOT show up as a ReadMessage return —
	// gorilla consumes it internally, inside ReadMessage/NextReader, before
	// handing control back to the caller (see gorilla's SetPongHandler doc:
	// "The handler function is called from the NextReader, ReadMessage and
	// message reader Read methods"). Without this handler, a peer that
	// faithfully pongs forever would still look silent to the read pump
	// below, and the deadline would fire on a perfectly healthy connection.
	// The handler always runs on whatever goroutine is inside ReadMessage —
	// here, always the read pump below, never the loop goroutine — so this
	// is a read-side call, not a write, and does not need to run on the loop
	// goroutine the way session's other methods do.
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readDeadline))
	})

	// connCtx is cancelled by this function's own teardown as well as by ctx,
	// so the read pump is released whether the loop ends because the caller
	// stopped it or because a frame handler failed.
	connCtx, connCancel := context.WithCancel(ctx)
	frames := make(chan []byte)
	readErr := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				select {
				case readErr <- err:
				default: // the loop is already leaving; nobody will read this
				}
				return
			}
			// Any inbound frame proves this connection is alive, not only a
			// pong (see runLoop's "Keepalive" doc section): refresh the same
			// deadline the pong handler above maintains, so a connection
			// carrying ordinary sync traffic — which may need no pings at
			// all — is never killed for silence it never actually had. This
			// runs on the same goroutine as ReadMessage and the pong handler
			// above (there is only ever one goroutine inside ReadMessage at
			// a time), so it introduces no new synchronisation concern of
			// its own.
			if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
				select {
				case readErr <- err:
				default:
				}
				return
			}
			select {
			case frames <- data:
			case <-connCtx.Done():
				return
			}
		}
	}()
	// Tear down in the order that actually unblocks the pump: cancel releases
	// a pump parked on the frames send, Close releases one parked in
	// ReadMessage, and only then is it safe to wait. Joining the goroutine
	// (rather than letting it drift) is what keeps `go test -race` from
	// attributing a later connection's activity to a leaked pump, and what
	// makes "no goroutine outlives Connect" checkable.
	defer func() {
		connCancel()
		_ = conn.Close()
		<-readerDone
	}()

	s := &session{c: c, conn: conn, onSynced: onSynced}
	c.emitStatus(Status{State: StateConnected})

	// pingTicker drives session.ping below, from THIS goroutine's own select
	// loop — see runLoop's "Keepalive" doc section for why here and not a
	// separate timer goroutine. defer covers every remaining exit from this
	// point on (the for/select's three return paths below), so this ticker's
	// underlying timer is always released when the connection is, and no
	// per-connection ticker survives past its own connection the way a
	// leaked one would show up under `go test -race` across repeated
	// reconnects.
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	// Open with our state vector, so the server can send back exactly what
	// this Doc is missing. Note ygo's own server does not wait to be asked:
	// it pushes SyncStep1 + a full SyncStep2 + awareness the moment the
	// upgrade completes (see provider/websocket's ServeHTTP), so in practice
	// its state usually arrives before this message is even read. Sending it
	// anyway is what makes this client correct against a y-websocket server
	// that DOES wait — and it costs one state vector.
	if err := s.write(encodeEnvelope(wireMsgSync, ygsync.EncodeSyncStep1(c.opts.Doc))); err != nil {
		return fmt.Errorf("client: send sync step 1: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return fmt.Errorf("client: read from %q: %w", c.opts.URL, err)
		case frame := <-frames:
			if err := s.handleFrame(frame); err != nil {
				return err
			}
		case <-c.lane.Signal():
			if err := s.flushLane(); err != nil {
				return err
			}
		case <-pingTicker.C:
			if err := s.ping(); err != nil {
				return fmt.Errorf("client: send ping: %w", err)
			}
		}
	}
}

// runReconnectLoop is Connect's actual dial loop: it runs runLoop over and
// over — dial, handshake, live sync, until that connection ends — applying
// a jittered backoff between failed attempts, until ctx is cancelled or the
// Client is closed (both collapse to ctx being done, by the time this is
// called; see Connect). It has no return value because it has nothing
// meaningful to return: it only ever exits once ctx is done, and every
// connection failure along the way is reported through OnStatus as it
// happens (as StateDisconnected{Err: err}) rather than surfaced here.
//
// Each reconnect re-runs runLoop's full handshake from scratch. That re-run
// is deliberately the ONLY recovery mechanism here: see flushLane's doc and
// the package doc for why an edit made while disconnected needs no separate
// replay path, and reconnect_test.go for the end-to-end proof.
//
// # Backoff: base 500ms, capped at Options.MaxBackoff, reset on handshake
//
// bo is a fresh backoff for the lifetime of this one Connect call — a
// Client is single-use (see Connect), so there is exactly one of these
// ever. It resets only when a connection's handshake actually completes
// (via the onSynced callback threaded into runLoop, which fires from
// handleFrame the moment the first SyncStep2 of a connection applies —
// see backoff.reset's own doc for why "handshake succeeded" and not merely
// "dial succeeded" is the bar). A run that fails before ever reaching that
// point — including one where the dial and WebSocket upgrade both
// succeeded but the handshake never got that far — leaves bo exactly where
// it was, so the NEXT attempt's delay keeps widening rather than resetting
// into a hot loop against a server that isn't actually serving anything.
func (c *Client) runReconnectLoop(ctx context.Context) {
	bo := backoff{base: reconnectBackoffBase, max: c.opts.MaxBackoff}

	for {
		err := c.runLoop(ctx, bo.reset)
		if ctx.Err() != nil {
			// runLoop only returns nil via its own ctx.Done() case, so
			// reaching here with ctx already done covers both a clean nil
			// return and — belt-and-suspenders, in case runLoop's contract
			// ever changes — a race where ctx happened to be cancelled at
			// almost the same moment a real failure occurred. Either way,
			// a deliberate stop is not something to report as a failure.
			return
		}

		c.emitStatus(Status{State: StateDisconnected, Err: err})

		delay := bo.next()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// write emits one already-framed message. Only ever called from the loop
// goroutine (see session's doc): this is the single-writer choke point.
func (s *session) write(frame []byte) error {
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return s.conn.WriteMessage(gws.BinaryMessage, frame)
}

// ping emits one WebSocket-level PING control frame, driving #165 Task 7's
// keepalive (see runLoop's "Keepalive" doc section for the full mechanism).
//
// Unlike write above, this does NOT go through SetWriteDeadline+WriteMessage
// — it uses WriteControl, which takes its own deadline argument and is the
// one gorilla write method documented safe to call concurrently with every
// other method, INCLUDING a WriteMessage the loop might currently be
// part-way through (see session's doc for exactly why that distinction is
// load-bearing, and what a keepalive built the OTHER way would have broken).
// It still only ever runs on the loop goroutine, called from runLoop's
// ticker case: WriteControl does not require that, but session's doc
// explains why keeping it there anyway is the simpler invariant to audit.
func (s *session) ping() error {
	return s.conn.WriteControl(gws.PingMessage, nil, time.Now().Add(writeTimeout))
}

// handleFrame dispatches one inbound y-websocket frame.
//
// A frame this client cannot parse or apply is DROPPED, not treated as a
// connection failure, mirroring provider/websocket's peer.handleMessage: one
// malformed or unappliable message is not evidence the socket is unusable,
// and tearing the connection down over it would turn a single bad frame into
// a reconnect storm. An error is returned only for a failure to WRITE, since
// that genuinely means this connection is finished.
func (s *session) handleFrame(frame []byte) error {
	msgType, payload, err := decodeEnvelope(frame)
	if err != nil {
		return nil
	}

	switch msgType {
	case wireMsgSync:
		// The sync payload follows the outer tag directly, with no VarBytes
		// wrapper (see encodeEnvelope). Read the sub-type before applying:
		// ApplySyncMessage does not report which kind of message it handled,
		// and "was that a SyncStep2?" is exactly what decides whether the
		// handshake is now complete.
		subType, _, subErr := ygsync.ReadSyncMessage(payload)
		reply, err := ygsync.ApplySyncMessage(s.c.opts.Doc, payload, s.c.remoteOrigin)
		if err != nil {
			return nil // unappliable — drop, see the method doc
		}
		if reply != nil {
			// The peer sent SyncStep1; reply is our SyncStep2 for it. This is
			// how our own offline edits reach the server: the server asks
			// what we have, and this answer carries everything it is missing.
			if err := s.write(encodeEnvelope(wireMsgSync, reply)); err != nil {
				return fmt.Errorf("client: send sync step 2 reply: %w", err)
			}
		}
		if subErr == nil && subType == ygsync.MsgSyncStep2 && !s.synced {
			s.synced = true
			s.c.markSynced()
			if s.onSynced != nil {
				s.onSynced()
			}
		}

	case wireMsgAwareness:
		// Awareness is a later #165 task's job. When it lands, note that
		// payload here is still VarBytes-WRAPPED — decodeEnvelope is
		// deliberately type-agnostic and strips only the outer tag — so it
		// must be unwrapped with encoding.NewDecoder(payload).ReadVarBytes()
		// before being handed to Awareness.ApplyUpdate, exactly as
		// provider/websocket/peer.go's `case msgAwareness` does. Dropping the
		// frame until then is harmless: awareness is idempotent heartbeat
		// state that the next update supersedes in full.
	}
	return nil
}

// flushLane drains every outbound payload the Doc observer (and, later,
// awareness) handed off and writes it to the socket.
//
// The Lane's signal is coalescing (capacity 1), so one wake-up can stand for
// any number of pushes: the drain must continue until both takes report
// empty rather than assuming one signal means one payload. A KindSync backlog
// comes back already merged into a single blob, which is why a burst of local
// edits costs one frame rather than one frame per Transact.
//
// # Take-before-write: safe for KindSync, NOT (yet) proven for awareness
//
// Each iteration TAKES a payload off the lane and only THEN writes it to the
// socket. If that write fails — the ordinary case for the failure this
// function is even capable of reporting, since Take itself cannot fail —
// the payload is already gone from the lane: it is not put back, and
// nothing else in this package retains a copy of it. In isolation that is a
// dropped update.
//
// For a KindSync payload (the TakeSync branch), this is NOT a lost update in
// practice, and the reason is load-bearing enough to say plainly rather than
// leave implicit: runReconnectLoop re-runs the full y-protocol handshake on
// every reconnect (see its doc), and that handshake's SyncStep2 is derived
// from the Doc's CURRENT state, not from the lane. The update this call just
// lost from the lane is still sitting in the Doc — Transact already applied
// it before onDocUpdate ever pushed it onto the lane — so the next
// successful connection's handshake sends it again, merged into whatever
// else changed meanwhile, with no special-casing required anywhere. This is
// precisely #165's central design point (there is no separate offline-op
// queue) applied to failure recovery as well as to planned disconnection: DO
// NOT "fix" the KindSync case by putting the payload back on a failed
// write, or by adding a retry queue here — that would be solving an
// already-solved problem, at the cost of a second source of truth for
// pending writes that could disagree with the Doc.
//
// For a TakeAwareness payload, the same argument does NOT hold, and a future
// implementer must not assume it does just because this comment is right
// above that branch too: awareness state is not doc state, so it is not
// part of what SyncStep1/SyncStep2 exchange, and a dropped awareness blob is
// not "still sitting" anywhere the handshake will ever look. This is latent
// today — nothing produces an awareness payload yet, so TakeAwareness never
// actually returns one until a later #165 task wires Awareness.SetLocalState
// (or similar) to push onto the lane — but latent is not the same as safe to
// ignore, because THIS comment is exactly what that task's implementer will
// read first. Whatever makes a dropped awareness write harmless will have to
// be awareness's own mechanism (its next local-state emit, or a periodic
// heartbeat, superseding the lost one — awareness is designed to be safely
// supersedable state, unlike a CRDT update) — confirm that property
// explicitly against the real implementation when that task lands, rather
// than inheriting this paragraph's KindSync conclusion by proximity.
func (s *session) flushLane() error {
	for {
		if update, ok := s.c.lane.TakeSync(); ok {
			if err := s.write(encodeEnvelope(wireMsgSync, ygsync.EncodeUpdate(update))); err != nil {
				return fmt.Errorf("client: send update: %w", err)
			}
			continue
		}
		if aw, ok := s.c.lane.TakeAwareness(); ok {
			// Awareness payloads ARE VarBytes-wrapped on the wire, unlike
			// sync payloads; encodeEnvelope appends whatever it is given
			// verbatim, so the wrapping is applied here.
			frame := encodeEnvelope(wireMsgAwareness, encoding.EncodeBytes(func(enc *encoding.Encoder) {
				enc.WriteVarBytes(aw)
			}))
			if err := s.write(frame); err != nil {
				return fmt.Errorf("client: send awareness: %w", err)
			}
			continue
		}
		return nil
	}
}

// markSynced records that the Doc has reconciled with the server at least
// once: it closes the Synced channel (once, ever — later connections
// re-handshake but the channel's contract is "has this Client EVER synced")
// and reports StateSynced, which does fire again on each new connection's
// handshake so a status subscriber can distinguish "connected" from
// "connected and caught up" every time.
func (c *Client) markSynced() {
	c.syncedOnce.Do(func() { close(c.synced) })
	c.emitStatus(Status{State: StateSynced})
}
