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

	// MaxBackoff caps the reconnect backoff the later dial loop applies
	// between failed attempts. Default: 30s.
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
// edits: hydration (LoadDoc → ApplyUpdateV1) and every update received from
// the server. The Doc observer Connect registers compares an update's origin
// against a single *remoteOrigin instance (this Client's c.remoteOrigin) by
// == to tell "this came from the network, so do not send it straight back"
// apart from "this is the caller's own edit, send it". Note it does NOT gate
// persistence: see onDocUpdate for why every update is stored regardless of
// origin.
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
// for the send-failure accounting a later #165 task adds with reconnect, and
// is always zero today.
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
	opts         Options
	room         string
	remoteOrigin *remoteOrigin
	awareness    *awareness.Awareness

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
		opts:         o,
		room:         room,
		remoteOrigin: &remoteOrigin{},
		awareness:    awareness.New(uint64(o.Doc.ClientID())),
		lane:         relaylane.New(0), // 0 = relaylane.DefaultCap
		synced:       make(chan struct{}),
		closed:       make(chan struct{}),
	}
	return c, nil
}

// onDocUpdate is the single Doc observer this Client registers, wired up by
// Connect after hydration and before the first dial (see Connect for why that
// exact position). It does the two things every applied update needs, and
// they are deliberately gated differently:
//
//   - PERSIST, whatever the origin. Both local edits and updates received
//     from the server go to the store. Persisting only local-origin updates
//     is a tempting simplification and a silent data-loss bug: a client that
//     syncs a large document, is closed, and reopens offline would hydrate
//     back only the edits it made itself, having thrown away everything it
//     ever learned from the server. Storing a remote update twice, by
//     contrast, costs nothing — V1 updates are idempotent.
//
//   - SEND, only when the origin is NOT this Client's remoteOrigin sentinel.
//     An update carrying that sentinel is one we just applied FROM the
//     server; bouncing it straight back would be pure echo. (Hydration also
//     uses the sentinel, but hydration cannot reach this observer at all,
//     because Connect registers it afterwards.)
//
// # Why this hands off instead of writing
//
// This runs on whichever goroutine called Transact — the embedding
// application's own goroutine, in the middle of its own edit. It must
// therefore never touch the socket: gorilla/websocket allows exactly one
// concurrent writer, and a write here would additionally park the
// application's edit behind network I/O. So it pushes onto the lane, which
// never blocks (a full lane merges its backlog rather than waiting or
// dropping), and returns. The loop goroutine picks the work up from there.
// provider/websocket removed this same head-of-line coupling server-side in
// #187; the client must not reintroduce it.
func (c *Client) onDocUpdate(update []byte, origin any) {
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
//  2. REGISTER the Doc observer — after hydration, so replaying the store's
//     own bytes is not mistaken for a fresh local edit and echoed onto the
//     wire, and before dialing, so no edit the caller makes from this moment
//     on can slip past unsent. (Edits made between New and this point are not
//     missed either, just carried differently: they are already in the Doc,
//     so the handshake's SyncStep2 delivers them wholesale.)
//
//  3. DIAL and run the sync loop.
//
// Connect is documented as blocking until stopped, and it keeps that promise
// even when the connection fails or ends: it reports the failure through
// OnStatus and then parks until ctx or Close releases it, rather than
// returning the error. Returning would make an unreachable server look like a
// terminal condition, when for an offline-first client it is the ordinary
// case — the Doc stays fully usable, and edits keep being persisted, with no
// server at all. A later #165 task fills this park with reconnect and
// backoff; the shape of Connect does not change when it does.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.hydrate(); err != nil {
		return err
	}

	unsub := c.opts.Doc.OnUpdate(c.onDocUpdate)
	defer unsub()

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

	err := c.runLoop(loopCtx)
	if loopCtx.Err() != nil {
		// Stopped on purpose (ctx cancelled or Close): not a failure.
		err = nil
	}
	c.emitStatus(Status{State: StateDisconnected, Err: err})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return nil
	}
}

// hydrate loads this Client's room from Store, if one was configured, and
// applies it to Doc under the remoteOrigin sentinel.
//
// The sentinel is not what stops the hydrated bytes being written straight
// back to the store — Connect not having registered its observer yet is (see
// Connect's step 2). It is used anyway because it is the truthful origin for
// this update, and because an application with its own Doc.OnUpdate observer
// can then tell hydration apart from a local edit exactly as it can tell a
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
	return crdt.ApplyUpdateV1(c.opts.Doc, blob, c.remoteOrigin)
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
// uses for this connection. Propagating that state over the wire is a later
// #165 task; the loop already carries awareness payloads outbound, but
// nothing produces them yet.
func (c *Client) Awareness() *awareness.Awareness {
	return c.awareness
}

// Close signals Connect to tear down its connection and return. It is safe to
// call more than once (only the first call has any effect) and safe to call
// concurrently with Connect.
//
// Close does not itself unregister the Doc observer: Connect owns that
// registration for exactly as long as it runs and removes it on the way out,
// which is both simpler than sharing the unsub function across goroutines and
// correct for a Client that was never connected in the first place (there is
// then nothing registered to remove). A caller that needs the observer
// definitely gone before proceeding should wait for Connect to return, which
// this call causes.
//
// Close does not close Store: the Store is constructed and owned by the
// caller (e.g. via OpenSQLiteStore), which may want to reuse it — for
// another Client, or simply to keep it open past this Client's lifetime —
// so closing it here would take that choice away from the caller.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
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
