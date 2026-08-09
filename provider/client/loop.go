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

// session is one connection's worth of loop state: the socket, and whatever
// must be remembered for the lifetime of THAT connection rather than the
// lifetime of the Client (currently just whether the handshake has completed).
// Splitting it out from Client is what lets the reconnect loop added by a
// later #165 task start a fresh session per attempt without having to
// remember which Client fields need resetting — a new connection is a new
// session, by construction.
//
// # Single-writer invariant
//
// Every method on session writes to conn, and every one of them runs on the
// loop goroutine that runLoop owns. That is not a coincidence to preserve
// case-by-case; it is the invariant this whole file is arranged around.
// gorilla/websocket permits only one concurrent writer, and — more
// importantly — a write that blocks on a slow socket must never be reachable
// from a caller's Transact. See Client.onDocUpdate for the other half of the
// arrangement (the observer hands off to a lane instead of writing).
type session struct {
	c    *Client
	conn *gws.Conn
	// synced records that this connection has applied a SyncStep2, so a
	// server that later sends another one (a resync, e.g. provider/
	// websocket's SlowPeerResync path) does not re-emit StateSynced as
	// though a new handshake had completed.
	synced bool
}

// runLoop performs one full connection lifecycle: dial, y-websocket
// handshake, then live bidirectional sync until ctx is cancelled or the
// connection fails. It returns nil for a clean stop (ctx cancelled) and an
// error describing what went wrong otherwise.
//
// Reconnect and backoff are a later #165 task's job. Today a failure simply
// ends the loop and Connect parks; see Connect's own doc for why parking (as
// opposed to returning the error) is the right placeholder.
//
// # Why the reads happen on a separate goroutine
//
// gorilla/websocket's ReadMessage blocks, and the loop must simultaneously be
// able to notice outbound work appearing on the lane. So a read pump
// goroutine does nothing but ReadMessage and hand frames to the loop over a
// channel; the loop goroutine remains the only writer. The read pump never
// touches the Doc, the lane, or the socket's write side, so it cannot violate
// the single-writer invariant no matter how it is scheduled.
func (c *Client) runLoop(ctx context.Context) error {
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

	s := &session{c: c, conn: conn}
	c.emitStatus(Status{State: StateConnected})

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
