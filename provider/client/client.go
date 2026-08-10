package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/relaylane"
)

// Defaults applied by New for any Options field left at its zero value. See
// Options for what each one controls; the values themselves are chosen to
// match provider/websocket's server-side equivalents (MaxMessageBytes /
// ReadLimit at 64 MiB, a 30s cadence for backoff ceiling and ping) so a
// client talking to ygo's own server needs no tuning to interoperate.
const (
	defaultMaxBackoff   = 30 * time.Second
	defaultPingInterval = 30 * time.Second
	defaultReadLimit    = int64(64 << 20)
	defaultCompactEvery = 500
)

// Options configures a Client. Doc and URL are required; every other field
// has a documented default applied by New when left at its zero value, so a
// caller can construct a working client from just those two.
type Options struct {
	// URL is the y-websocket server address to dial, e.g.
	// "wss://example.com/yjs/my-room". The final path segment is extracted
	// as the room/document name (see roomFromURL) — the same name the
	// server's provider/websocket.Server route table keys rooms by.
	URL string

	// Doc is the document the client hydrates, edits, and keeps in sync.
	// Required: New rejects a nil Doc, since every other operation this
	// client performs (hydration, later the dial loop) needs a Doc to act
	// on, and there is no sensible default for a caller's own document.
	Doc *crdt.Doc

	// Store is where the client persists local edits and hydrates prior
	// ones from, so a Doc's content survives a process restart while
	// offline (see the package doc for why this is the client's only job
	// the sync protocol itself can't do). Nil is valid and means "no local
	// durability" — the Doc is still fully usable in-memory, it just starts
	// empty on every restart and holds nothing once Close returns, exactly
	// like using a *crdt.Doc directly without this package.
	Store LocalStore

	// Token is the Hocuspocus in-band auth token (mirrors
	// provider/websocket's OnTokenAuth, #104). The in-band auth exchange
	// that sends it is a later #165 task; the dial loop does not send it
	// yet. Use Header for HTTP-level credentials in the meantime.
	Token string

	// Header carries additional HTTP headers on the WebSocket upgrade
	// request (e.g. a bearer token in Authorization, ahead of or instead of
	// Token).
	//
	// New takes a defensive copy: mutating the map after New returns has no
	// effect on what the client dials with. Without the copy a Client would
	// alias the caller's map and every dial — including reconnects on a
	// goroutine the caller knows nothing about — would read it concurrently
	// with whatever the caller does to it next, which is a data race the
	// caller has no way to see coming.
	Header http.Header

	// MaxBackoff caps the reconnect backoff the dial loop applies between
	// failed attempts. Default: 30s.
	MaxBackoff time.Duration

	// PingInterval is the cadence at which the later dial loop sends
	// liveness pings on an established connection, mirroring
	// provider/websocket's own ping/pong keepalive. Default: 30s.
	PingInterval time.Duration

	// ReadLimit caps the size of a single frame the dial loop will accept
	// from the server, mirroring provider/websocket.Server's MaxMessageBytes
	// so a client and ygo's own server agree on the ceiling. Default: 64 MiB
	// (64<<20).
	ReadLimit int64

	// CompactEvery is how many stored updates accumulate (per room) before
	// the later sync loop asks a CompactableStore to collapse its log,
	// mirroring provider/websocket.Server.CompactEvery. Ignored for a Store
	// that does not implement CompactableStore. Default: 500.
	CompactEvery int
}

// remoteOrigin is the sentinel origin type stamped on every update the
// client applies to its Doc that did NOT originate from the caller's own
// edits, and specifically on those received from the SERVER — hydration has
// its own sentinel, see hydrateOrigin. The Doc observer New registers compares
// an update's origin against a single *remoteOrigin instance (this Client's
// c.remoteOrigin) by == to tell "this came from the network, so do not send it
// straight back" apart from "this is the caller's own edit, send it". It does
// NOT suppress persistence: a server-received update must be stored, or a
// client that syncs and then restarts offline loses everything it learned. See
// onDocUpdate for the full three-way rule.
//
// This type MUST stay a non-zero-size struct (the `_ byte` field is load-
// bearing — do not remove it, and do not "simplify" this back to
// `struct{}`). Go's size-and-alignment guarantee lets the runtime satisfy
// every *zero-size* allocation from the same backing address
// (runtime.zerobase), so two unrelated `new(struct{})` values anywhere in the
// process compare == to each other even though nothing about them is
// actually the same value. That exact aliasing disabled every
// provider/websocket relay publish for six releases (see
// relayOriginSentinel's doc comment in provider/websocket/cluster.go, and
// #203 for the public-API twin of the same bug in WithTrackedOrigins) before
// it was understood. Giving this sentinel its own named, non-zero-size type
// removes the risk structurally — each *remoteOrigin gets its own heap
// allocation — rather than relying on every future caller remembering not to
// use a bare struct{}.
type remoteOrigin struct{ _ byte }

// hydrateOrigin is the sentinel origin stamped on the one update hydration
// applies (LoadDoc → ApplyUpdateV1). It is deliberately a SEPARATE type from
// remoteOrigin even though both mean "not the caller's own edit", because the
// two need opposite persistence answers: a hydrated update is already in the
// store by definition, while a server-received one must be written to it. One
// shared sentinel cannot express that, and the alternative — inferring it from
// whether hydration has finished yet — would make the policy a consequence of
// when a line of code runs rather than something stated outright. See
// onDocUpdate for the resulting three-way rule.
//
// Like remoteOrigin, this MUST stay a non-zero-size struct; see remoteOrigin's
// doc comment for the zerobase aliasing this prevents, and note that two bare
// `struct{}` sentinels would additionally have aliased EACH OTHER here, which
// is precisely the distinction this type exists to make.
type hydrateOrigin struct{ _ byte }

// ErrAlreadyConnected is returned by Connect when this Client has already had
// a Connect call accepted. A Client is single-use.
var ErrAlreadyConnected = errors.New("ygo/client: already connected")

// State is a Client's coarse connection lifecycle, reported via Status.
type State int

const (
	// StateConnecting means a dial or handshake attempt is in flight.
	StateConnecting State = iota
	// StateConnected means the WebSocket connection is up but the initial
	// sync handshake (SyncStep1/SyncStep2) has not yet completed.
	StateConnected
	// StateSynced means the sync handshake has completed at least once on
	// the current connection: the client's Doc has reconciled with the
	// server's state as of connect time.
	StateSynced
	// StateDisconnected means there is no live connection and no attempt is
	// currently in flight (between backoff attempts, or after Close).
	StateDisconnected
)

// Status is what OnStatus subscribers receive: the Client's current State,
// and — for StateDisconnected transitions caused by a failure rather than a
// clean Close — the error that caused it.
type Status struct {
	State State
	Err   error
}

// Stats are cumulative counters a caller can poll to understand what the
// client's sync loop has been doing without wiring a full OnStatus
// subscription. Coalesced, AwarenessSuperseded and HardDrops come from the
// outbound lane (see internal/relaylane) and are live; Dropped is reserved
// for the send-failure accounting a later #165 task (Stats/Close hardening)
// adds, and is always zero today — reconnecting (see runReconnectLoop) does
// not by itself lose anything a Dropped counter would need to report: a
// failed connection's unsent lane contents are superseded by the next
// connection's full handshake resync (see flushLane's doc), not discarded.
type Stats struct {
	// Coalesced counts local updates that were merged into a pending batch
	// rather than sent as a separate wire message, mirroring
	// provider/websocket's persistence-coalescing counters.
	Coalesced uint64
	// AwarenessSuperseded counts locally-set awareness states that were
	// superseded by a newer local SetLocalState call before ever being sent.
	AwarenessSuperseded uint64
	// HardDrops counts updates the client gave up retrying and discarded
	// outright (e.g. exceeding a retry/staleness bound).
	HardDrops uint64
	// Dropped counts updates lost for any other reason, including transient
	// send failures superseded by a subsequent successful send.
	Dropped uint64
}

// statusSub is one OnStatus subscriber. Subscriptions are identified by an
// increasing id, not by slice position, so unsubscribing is safe even when
// other subscribers have been added or removed out of order in the
// meantime — the same pattern crdt.Doc.OnUpdate uses, chosen for the same
// reason: capturing an index instead of an id lets a later removal shift the
// slice out from under an unrelated unsubscribe call.
type statusSub struct {
	id uint64
	fn func(Status)
}

// Client is an embeddable, offline-first sync client for a single *crdt.Doc.
// Construct one with New and drive it with Connect; see the package doc for
// the hydrate-before-dial design this type exists to provide.
//
// A Client's exported methods are safe for concurrent use.
type Client struct {
	opts          Options
	room          string
	remoteOrigin  *remoteOrigin
	hydrateOrigin *hydrateOrigin
	awareness     *awareness.Awareness

	// unsubObserver removes the Doc observer New registered. Set once, in
	// New, before this Client is reachable by any other goroutine, and read
	// only by Close — so it needs no synchronisation of its own.
	unsubObserver func()

	// unsubAwareness removes the Awareness.OnUpdate observer New registered
	// (see onAwarenessUpdate). Same lifecycle and same "no synchronisation
	// needed" reasoning as unsubObserver above.
	unsubAwareness func()

	// connectStarted latches when a Connect call is accepted, so a second one
	// is refused rather than starting a rival dial loop and a rival observer
	// on the same Doc. See Connect and ErrAlreadyConnected.
	connectStarted atomic.Bool

	// lane is the hand-off between the Doc observer (which runs on whichever
	// goroutine called Transact) and the loop goroutine (the socket's only
	// writer). See onDocUpdate for why the hand-off has to exist at all, and
	// internal/relaylane for its bounded, coalescing, never-blocking policy.
	// It is created in New rather than per-connection so local edits made
	// while offline queue up for the next connection instead of needing a
	// separate holding pen.
	lane *relaylane.Lane

	statusMu       sync.Mutex
	statusSubs     []statusSub
	statusSubIDGen uint64

	synced     chan struct{}
	syncedOnce sync.Once
	closed     chan struct{}
	closeOnce  sync.Once

	// statsDropped backs Stats.Dropped. The other three Stats fields are read
	// straight off the lane (see Stats), which is the only place that
	// accounting actually happens; duplicating them here would just be a
	// second copy to keep in step.
	statsDropped atomic.Uint64
}

// roomFromURL extracts the room/document name from a y-websocket URL: the
// final path segment, percent-decoded (net/url.Parse decodes it into
// u.Path already; this just isolates the last segment of that). This is the
// same name a provider/websocket.Server route table keys rooms by (see
// Server.ServeHTTP's use of r.PathValue("room") / path.Base(r.URL.Path)), so
// a URL built for this client's Connect names the same room an app would use
// dialing provider/websocket directly.
//
// An error is returned for a URL net/url cannot parse, or one with no room
// segment at all (empty path, or a path of just "/") — either is a caller
// mistake New must reject at construction rather than let surface later as
// an inscrutable dial failure.
func roomFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("client: invalid URL %q: %w", raw, err)
	}
	room := path.Base(u.Path)
	if room == "" || room == "." || room == "/" {
		return "", fmt.Errorf("client: URL %q has no room path segment", raw)
	}
	return room, nil
}

// New validates o and returns a Client ready to hydrate and (once later
// #165 tasks land) dial. It does not touch the network or the Store; call
// Connect to hydrate from Store and begin syncing.
//
// New rejects a nil Doc and a URL that net/url cannot parse or that has no
// room path segment (see roomFromURL); every other Options field is
// optional and defaulted (see Options's field docs for the defaults
// applied).
func New(o Options) (*Client, error) {
	if o.Doc == nil {
		return nil, errors.New("client: Options.Doc must not be nil")
	}
	room, err := roomFromURL(o.URL)
	if err != nil {
		return nil, err
	}

	if o.MaxBackoff <= 0 {
		o.MaxBackoff = defaultMaxBackoff
	}
	if o.PingInterval <= 0 {
		o.PingInterval = defaultPingInterval
	}
	if o.ReadLimit <= 0 {
		o.ReadLimit = defaultReadLimit
	}
	if o.CompactEvery <= 0 {
		o.CompactEvery = defaultCompactEvery
	}
	// Defensive copy so the Client never aliases the caller's map; see
	// Options.Header. Clone returns nil for a nil Header, which is what the
	// dialer wants anyway.
	o.Header = o.Header.Clone()

	c := &Client{
		opts:          o,
		room:          room,
		remoteOrigin:  &remoteOrigin{},
		hydrateOrigin: &hydrateOrigin{},
		awareness:     awareness.New(uint64(o.Doc.ClientID())),
		lane:          relaylane.New(0), // 0 = relaylane.DefaultCap
		synced:        make(chan struct{}),
		closed:        make(chan struct{}),
	}
	// Register the Doc observer HERE, at the earliest possible moment, rather
	// than somewhere inside Connect. An offline-first client's Doc is usable
	// the instant New returns, so an application may reasonably edit it before
	// it ever calls Connect (or without calling Connect at all); registering
	// later would leave every such edit absent from the store and lost on the
	// next restart. Nothing about the observer needs a connection to be
	// correct — what an update is FOR is decided by its origin, not by when it
	// arrives (see onDocUpdate).
	c.unsubObserver = o.Doc.OnUpdate(c.onDocUpdate)
	// Register the Awareness observer here for the same reason as the Doc
	// observer just above: an offline-first client's Awareness is usable
	// (SetLocalState, Heartbeat) the instant New returns, before Connect is
	// ever called, and a caller doing so before Connect must not have that
	// state silently dropped — it should simply queue on the lane (see
	// onAwarenessUpdate) until a connection exists to flush it over.
	c.unsubAwareness = c.awareness.OnUpdate(c.onAwarenessUpdate)
	return c, nil
}

// onDocUpdate is the single Doc observer this Client registers, wired up by
// New so that it is in place before the caller can possibly make an edit.
//
// It answers two independent questions about every update applied to the Doc —
// "should this be stored?" and "should this be sent?" — and it answers both
// purely from the update's ORIGIN. That is deliberate, and the reason there
// are three sentinels' worth of distinction rather than two:
//
//	origin          store?  send?  because
//	--------------  ------  -----  ---------------------------------------
//	hydrateOrigin   no      no     it came OUT of the store, and the
//	                               handshake conveys full state anyway
//	remoteOrigin    yes     no     came from the server: must be durable,
//	                               must not be echoed back at it
//	anything else   yes     yes    the caller's own edit
//
// Storing remote updates is the non-obvious one, and skipping them would be a
// silent data-loss bug: a client that syncs a large document, closes, and
// reopens offline would hydrate back only the edits it made itself, having
// discarded everything it ever learned from the server. Storing one twice, by
// contrast, costs nothing — V1 updates are idempotent.
//
// Expressing all of this as origin policy rather than as "whatever happens to
// be registered at the time" is what lets the observer live in New. The
// earlier alternative — one sentinel for both hydration and the network, with
// hydration kept out of the store by registering this observer part-way
// through Connect — worked, but only by making a correctness rule depend on
// statement order, and it left every edit made between New and Connect
// unpersisted.
//
// # Why this hands off instead of writing
//
// This runs on whichever goroutine called Transact — the embedding
// application's own, in the middle of its own edit. It must therefore never
// touch the socket: gorilla/websocket allows exactly one concurrent writer,
// and a write here would additionally park the application's edit behind
// network I/O. So it pushes onto the lane, which never blocks (a full lane
// merges its backlog rather than waiting or dropping), and returns. The loop
// goroutine picks the work up from there. provider/websocket removed this same
// head-of-line coupling server-side in #187; the client must not reintroduce
// it.
func (c *Client) onDocUpdate(update []byte, origin any) {
	if origin == c.hydrateOrigin {
		return
	}
	if c.opts.Store != nil {
		// Best-effort: there is no failure channel for a store write yet (no
		// Stats field or Status shape fits it), so an error here is dropped.
		// A later #165 task that adds real failure reporting should revisit.
		_ = c.opts.Store.StoreUpdate(c.room, update)
	}
	if origin == c.remoteOrigin {
		return
	}
	// The lane retains this slice for an unbounded time, and the same slice
	// is handed to every other OnUpdate observer on this Doc (including the
	// application's own). Copying is one allocation per local transaction and
	// removes any question of who may touch it afterwards.
	queued := make([]byte, len(update))
	copy(queued, update)
	c.lane.Push(cluster.KindSync, queued)
}

// onAwarenessUpdate is the single Awareness.OnUpdate observer this Client
// registers, wired up by New for the same "usable before Connect" reason
// onDocUpdate is (#165 Task 8).
//
// Unlike onDocUpdate's three-way origin table, awareness has no store and no
// hydration, so there is only one distinction that matters: did this update
// originate from OUR OWN awareness calls (SetLocalState, Heartbeat — origin
// nil) or was it applied because the NETWORK told us something (ApplyUpdate,
// always called with origin c.remoteOrigin — see handleFrame's wireMsgAwareness
// case, and dropRemoteAwareness's own local-bookkeeping use of the same
// sentinel below)? Only the former should ever be sent back out; echoing the
// latter would bounce every remote peer's presence back at the server
// forever, the awareness analogue of the doc-side echo storm
// TestClient_SuppressesEchoAfterConvergence (offline_test.go) guards against.
//
// Only the affected client IDs are re-encoded and pushed, not the Client's
// entire known state (EncodeUpdate(nil)) — SetLocalState and Heartbeat only
// ever touch the local clientID, so evt's Added/Updated/Removed already is
// that one ID in every real call; encoding just those IDs keeps the wire
// message from also re-broadcasting every remote entry's unchanged state on
// every local presence tick.
func (c *Client) onAwarenessUpdate(evt awareness.UpdateEvent) {
	if evt.Origin == c.remoteOrigin {
		return
	}
	n := len(evt.Added) + len(evt.Updated) + len(evt.Removed)
	if n == 0 {
		return
	}
	ids := make([]uint64, 0, n)
	ids = append(ids, evt.Added...)
	ids = append(ids, evt.Updated...)
	ids = append(ids, evt.Removed...)
	c.lane.Push(cluster.KindAwareness, c.awareness.EncodeUpdate(ids))
}

// Connect hydrates the Client's Doc from Store (if one was configured),
// starts syncing with the server, and blocks until ctx is cancelled or Close
// is called. It returns ctx.Err() for the former and nil for the latter.
//
// The three things it does happen in this order for reasons, and the order is
// part of the contract:
//
//  1. HYDRATE, synchronously, before anything touches the network. This is
//     the hydrate-before-dial ordering the package doc describes: an app that
//     calls Connect against an unreachable server still gets its
//     previously-persisted content applied to Doc, because hydration never
//     depended on the network to begin with.
//
//  2. DIAL and run the sync loop.
//
// (There is no "register the observer" step: New did that, so an edit made
// before Connect was ever called is already durable. See onDocUpdate.)
//
// Connect is documented as blocking until stopped, and it keeps that promise
// even when a connection fails: it reports the failure through OnStatus as
// StateDisconnected, then — rather than returning the error, or parking for
// good — waits out a jittered backoff and dials again, indefinitely, until
// ctx is cancelled or Close is called. Returning on the first failure would
// make an unreachable server look like a terminal condition, when for an
// offline-first client it is the ordinary case: the Doc stays fully usable,
// and edits keep being persisted, with no server reachable at all. See
// runReconnectLoop (loop.go) for the retry loop itself and backoff (backoff.go)
// for the delay schedule between attempts.
//
// Every reconnect re-runs the full y-protocol handshake from scratch. That is
// deliberately ALL reconnect does: there is no separate offline-op queue
// anywhere in this client. An edit made while disconnected sits in the Doc
// (and, redundantly, in the outbound lane — see flushLane's doc for why that
// redundancy is harmless) until the next connection's SyncStep1/SyncStep2
// exchange, whose whole job is declaring what each side has and sending
// whatever the other is missing. That exchange is what carries the edit to
// the server; nothing here replays it.
//
// # Single use
//
// A Client accepts one Connect. A second call — concurrent or sequential,
// before or after Close — returns ErrAlreadyConnected rather than starting a
// rival dial loop against the same Doc and Store, which would mean two
// sockets, every local edit stored and sent twice, and two goroutines each
// believing itself the sole writer of its socket. Reconnection after a drop is
// entirely internal to this one Connect call's own loop, not something a
// caller drives by calling Connect again; a caller that genuinely wants a
// fresh session should construct a fresh Client.
//
// A Connect that fails during hydration is the exception: it started nothing,
// so it does not latch, and the call may be retried once whatever the Store
// was unhappy about is resolved.
func (c *Client) Connect(ctx context.Context) error {
	if !c.connectStarted.CompareAndSwap(false, true) {
		return ErrAlreadyConnected
	}
	if err := c.hydrate(); err != nil {
		// Nothing has been started, so release the guard: unlike every later
		// failure, this one leaves the Client exactly as New returned it.
		c.connectStarted.Store(false)
		return err
	}

	// Fold Close into the loop's context so the loop has exactly one stop
	// signal to watch instead of two.
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcherDone := make(chan struct{})
	defer close(watcherDone)
	go func() {
		select {
		case <-c.closed:
			cancel()
		case <-watcherDone:
		}
	}()

	// runReconnectLoop only ever returns once loopCtx is done (see its own
	// doc): every connection failure along the way is already reported
	// through OnStatus as it happens, so there is nothing left to inspect
	// once it returns. The final, unconditional emission below is the
	// bookend StateDisconnected's own doc describes ("after Close") — it is
	// deliberately Err: nil even if the very last attempt before the stop
	// had failed, because THAT failure was already reported with its real
	// error by runReconnectLoop; this one specifically means "and now the
	// client is stopped, on purpose."
	c.runReconnectLoop(loopCtx)
	c.emitStatus(Status{State: StateDisconnected})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return nil
	}
}

// hydrate loads this Client's room from Store, if one was configured, and
// applies it to Doc under the hydrateOrigin sentinel.
//
// That sentinel is load-bearing, and it is specifically the reason hydration
// needs one of its own rather than sharing remoteOrigin. The Doc observer is
// registered by New, so it is already live when this runs and WILL see this
// update; the only thing stopping it writing the store's own bytes straight
// back to the store — on every start, forever — is recognising the origin.
// remoteOrigin could not serve here, because updates carrying THAT origin must
// be persisted (see onDocUpdate's table).
//
// It also gives an application with its own Doc.OnUpdate observer a way to
// tell hydration apart from a local edit, exactly as it can tell a
// server-received update apart from one.
//
// A Store with no prior state for this room returns (nil, nil) per
// LocalStore's contract; hydrate treats that as a no-op rather than an
// error, since "nothing stored yet" is the ordinary state of a brand-new
// room.
func (c *Client) hydrate() error {
	if c.opts.Store == nil {
		return nil
	}
	blob, err := c.opts.Store.LoadDoc(c.room)
	if err != nil {
		return fmt.Errorf("client: hydrate room %q: %w", c.room, err)
	}
	if len(blob) == 0 {
		return nil
	}
	return crdt.ApplyUpdateV1(c.opts.Doc, blob, c.hydrateOrigin)
}

// Synced returns a channel that is closed the first time this Client's Doc
// reconciles with the server on a live connection: specifically, when the
// first SyncStep2 of a connection has been applied. It never closes for a
// Client that has not completed a sync handshake — a Client that never
// dialed, or that has only ever failed to reach its server, leaves it open
// forever. It closes at most once; a reconnect re-emits StateSynced through
// OnStatus but does not (and cannot) reopen the channel.
//
// Callers that want "block until this doc has whatever the server has, at
// least once" should select on the returned channel rather than polling
// OnStatus for StateSynced.
func (c *Client) Synced() <-chan struct{} {
	return c.synced
}

// OnStatus registers fn to be called with every Status this Client reports
// from here on (no replay of past statuses). It returns an unsub function
// that removes fn; unsub is safe to call concurrently, safe to call more
// than once, and safe to call from inside fn itself or from inside another
// subscriber's callback — subscriptions are matched by an internal id, not
// slice position (see statusSub), and emitStatus snapshots the subscriber
// list and releases its lock before invoking any callback, so a callback
// can freely subscribe or unsubscribe without deadlocking against the lock
// emitStatus itself needs. This mirrors the no-lock-held-during-callbacks
// rule crdt.Doc.OnUpdate and provider/websocket's observers already follow.
func (c *Client) OnStatus(fn func(Status)) (unsub func()) {
	c.statusMu.Lock()
	c.statusSubIDGen++
	id := c.statusSubIDGen
	c.statusSubs = append(c.statusSubs, statusSub{id: id, fn: fn})
	c.statusMu.Unlock()

	return func() {
		c.statusMu.Lock()
		for i, s := range c.statusSubs {
			if s.id == id {
				c.statusSubs = append(c.statusSubs[:i], c.statusSubs[i+1:]...)
				break
			}
		}
		c.statusMu.Unlock()
	}
}

// emitStatus fans st out to every current OnStatus subscriber. It snapshots
// the subscriber list under statusMu and releases the lock before calling
// any of them, so a subscriber callback that calls back into this Client
// (OnStatus, unsub, or — in later tasks — anything else that takes
// statusMu) cannot deadlock against emitStatus's own lock. This mirrors the
// no-lock-held-during-callbacks rule crdt.Doc.OnUpdate and
// provider/websocket's observers already follow.
//
// Every call site is on the loop goroutine (or on Connect's own goroutine
// around it), so a subscriber that blocks stalls this Client's sync — the
// same contract provider/websocket's hooks carry, and the reason those hooks
// are documented as "do not block".
func (c *Client) emitStatus(st Status) {
	c.statusMu.Lock()
	subs := make([]statusSub, len(c.statusSubs))
	copy(subs, c.statusSubs)
	c.statusMu.Unlock()

	for _, s := range subs {
		s.fn(st)
	}
}

// Awareness returns this Client's Awareness instance, keyed to the same
// ClientID as Doc — so presence/cursor state a caller sets locally via
// SetLocalState is attributed to the same peer identity the sync protocol
// uses for this connection.
//
// Local state set here (or updated via Heartbeat) propagates to the server —
// and from there, to every other peer in the room — via onAwarenessUpdate,
// the observer New registers on this same instance: it holds regardless of
// whether Connect has been called yet, exactly like Doc edits (see New's
// doc). Once connected, it is additionally kept alive automatically: every
// successful handshake (including a reconnect's) and every PingInterval tick
// re-announces it at a bumped clock, so a quiet client is not reaped by a
// server's AwarenessExpiry sweep (see runLoop's ping ticker case and its
// sync-step-2 completion branch).
func (c *Client) Awareness() *awareness.Awareness {
	return c.awareness
}

// dropRemoteAwareness marks every REMOTE client's state in this Client's own
// Awareness as removed, leaving only the local client's own entry intact.
// Called once per connection teardown (see runLoop's defer) to mirror
// y-websocket's WebsocketProvider.removeAwarenessStates on disconnect:
// presence learned over a socket that no longer exists is stale the instant
// the socket dies, and a caller polling GetStates afterwards should not keep
// seeing peers this Client can no longer actually hear from.
//
// The removal is applied via Awareness.ApplyUpdate carrying c.remoteOrigin —
// deliberately the SAME sentinel handleFrame's wireMsgAwareness case stamps
// on every network-applied update, not a new one-off flag — specifically so
// onAwarenessUpdate's existing "do not re-send what came from the network"
// rule (see its doc) already covers this for free. Without that, this
// purely-local bookkeeping change would sit on the lane and get flushed to
// the NEXT connection as if it were real news, telling a possibly brand-new
// room about client IDs it has never heard of and, for a room that DID
// survive the disconnect, incorrectly announcing peers who are still
// perfectly present as removed.
//
// This builds the removal update by hand (WriteVarUint/WriteVarString)
// rather than via Awareness.EncodeUpdate, because EncodeUpdate always
// encodes each client's CURRENTLY STORED state — there is no "force this ID
// to null" mode — whereas a removal update's whole point is to override
// that stored state.
//
// Each entry's clock is the client's CURRENT last-known clock, deliberately
// NOT bumped by one. GetStates only returns entries that are still active
// (State != nil, see its own doc), so every id here satisfies
// ApplyUpdate's "equal clock + null + currently-active → accept" rule
// as-is — no increment is needed to clear that gate for THIS call. Bumping
// it anyway (an earlier version of this method did) creates a poisoned
// tombstone: if the remote peer's own next real update happens to carry
// that exact same clock+1 (its ordinary Heartbeat/SetLocalState bump from
// the same last-known clock this method just read), it arrives as a
// non-null entry at a clock EQUAL to this tombstone's — and ApplyUpdate's
// equal-clock gate only accepts a null entry overriding a non-null current,
// never the reverse, so the peer's genuine reappearance would be silently
// dropped. Leaving the clock unbumped avoids the collision entirely: the
// peer's real next update is only ever accepted by the ordinary
// strictly-newer-clock path once it actually arrives.
func (c *Client) dropRemoteAwareness() {
	states := c.awareness.GetStates()
	localID := c.awareness.ClientID()

	var ids []uint64
	for id := range states {
		if id != localID {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}

	enc := encoding.NewEncoder()
	enc.WriteVarUint(uint64(len(ids)))
	for _, id := range ids {
		enc.WriteVarUint(id)
		enc.WriteVarUint(states[id].Clock)
		enc.WriteVarString("null")
	}
	// ApplyUpdate's only error is a malformed update; this one is built
	// correctly by construction, so there is nothing a caller could do with
	// an error here that isn't already implied by "removal didn't happen" —
	// which is itself harmless (see this method's doc: the entries are
	// stale local bookkeeping, not data anything depends on).
	_ = c.awareness.ApplyUpdate(enc.Bytes(), c.remoteOrigin)
}

// Close signals Connect to tear down its connection and return. It is safe to
// call more than once (only the first call has any effect) and safe to call
// concurrently with Connect.
//
// It also unregisters the Doc observer AND the Awareness observer New
// installed, so a Doc/Awareness pair that outlives its Client stops writing
// to the Store, stops queueing doc sends, and stops queueing awareness
// re-announcements that nothing will ever drain. Both unsub functions are
// written once, in New, before the Client is reachable by any other
// goroutine, and read only here under closeOnce — so this needs no lock and
// cannot race Connect.
//
// Close does not close Store: the Store is constructed and owned by the
// caller (e.g. via OpenSQLiteStore), which may want to reuse it — for
// another Client, or simply to keep it open past this Client's lifetime —
// so closing it here would take that choice away from the caller.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.unsubObserver()
		c.unsubAwareness()
	})
	return nil
}

// Stats returns a snapshot of this Client's cumulative counters. See Stats
// for what each field means.
//
// Three of the four are read straight off the outbound lane, which is where
// the coalescing they describe actually happens — deliberately, rather than
// mirroring them into Client-side atomics that would then have to be kept in
// step with the lane's own accounting. Lane.Stats is lock-free, so this is
// safe to poll at any rate and from any goroutine, including from inside an
// OnStatus callback.
func (c *Client) Stats() Stats {
	ls := c.lane.Stats()
	return Stats{
		Coalesced:           ls.Coalesced,
		AwarenessSuperseded: ls.AwarenessSuperseded,
		HardDrops:           ls.HardDrops,
		Dropped:             c.statsDropped.Load(),
	}
}
