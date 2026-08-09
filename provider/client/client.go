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
	"github.com/reearth/ygo/crdt"
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

	// Token is carried through to later tasks' dial handshake as the
	// Hocuspocus in-band auth token (mirrors provider/websocket's
	// OnTokenAuth, #104). Unused by this task's Connect, which does not
	// dial yet.
	Token string

	// Header is carried through to later tasks' dial as additional HTTP
	// headers on the WebSocket upgrade request (e.g. a bearer token in
	// Authorization, ahead of or instead of Token). Unused by this task's
	// Connect.
	Header http.Header

	// MaxBackoff caps the reconnect backoff the later dial loop applies
	// between failed attempts. Default: 30s.
	MaxBackoff time.Duration

	// PingInterval is the cadence at which the later dial loop sends
	// liveness pings on an established connection, mirroring
	// provider/websocket's own ping/pong keepalive. Default: 30s.
	PingInterval time.Duration

	// ReadLimit caps the size of a single frame the later dial loop will
	// accept from the server, mirroring provider/websocket.Server's
	// MaxMessageBytes so a client and ygo's own server agree on the ceiling.
	// Default: 64 MiB (64<<20).
	ReadLimit int64

	// CompactEvery is how many stored updates accumulate (per room) before
	// the later sync loop asks a CompactableStore to collapse its log,
	// mirroring provider/websocket.Server.CompactEvery. Ignored for a Store
	// that does not implement CompactableStore. Default: 500.
	CompactEvery int
}

// remoteOrigin is the sentinel origin type stamped on every update the
// client applies to its Doc that did NOT originate from the caller's own
// edits — currently just hydration (LoadDoc → ApplyUpdateV1), and, once the
// later sync-loop tasks land, every update received from the server. The
// persistence hook New registers on the Doc compares an update's origin
// against a single *remoteOrigin instance (this Client's c.remoteOrigin) by
// == to tell "this update is already durable, or came from the network, so
// don't feed it back into the store" apart from "this is the caller's own
// edit, persist it".
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
// subscription. All fields are populated by later #165 tasks (the dial loop,
// awareness merging, and their failure paths); this task defines the shape
// and returns it correctly zeroed, since no loop runs yet.
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

	unsubPersist func()

	statusMu       sync.Mutex
	statusSubs     []statusSub
	statusSubIDGen uint64

	synced    chan struct{}
	closed    chan struct{}
	closeOnce sync.Once

	statsCoalesced           atomic.Uint64
	statsAwarenessSuperseded atomic.Uint64
	statsHardDrops           atomic.Uint64
	statsDropped             atomic.Uint64
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

	c := &Client{
		opts:         o,
		room:         room,
		remoteOrigin: &remoteOrigin{},
		awareness:    awareness.New(uint64(o.Doc.ClientID())),
		synced:       make(chan struct{}),
		closed:       make(chan struct{}),
	}
	c.unsubPersist = o.Doc.OnUpdate(c.persistLocalUpdate)
	return c, nil
}

// persistLocalUpdate is registered on Doc via OnUpdate at construction time
// (before any hydration happens) so that every locally-originated
// transaction is durable before it matters, matching the ordering
// LocalStore's own doc comment describes for provider/websocket's
// persistence worker.
//
// It must skip updates whose origin is this Client's remoteOrigin sentinel:
// those are updates the client itself applied from the store (hydration) or,
// once the later sync-loop tasks land, from the server — writing them back
// to the store would be redundant at best (hydration) and would corrupt the
// "who edited this" picture at worst (echoing a server update back in as if
// it were a fresh local edit).
func (c *Client) persistLocalUpdate(update []byte, origin any) {
	if origin == c.remoteOrigin {
		return
	}
	if c.opts.Store == nil {
		return
	}
	// Best-effort: this task builds no sync loop to surface a persistence
	// failure through (no OnStatus emission exists yet for it), so there is
	// nothing more useful to do with the error here than drop it. Later
	// #165 tasks that add real failure reporting should reconsider this.
	_ = c.opts.Store.StoreUpdate(c.room, update)
}

// Connect hydrates the Client's Doc from Store (if one was configured) and
// then blocks until ctx is cancelled or Close is called.
//
// Hydration happens synchronously, before Connect does anything else — in
// particular, before any attempt to reach the server, once later #165 tasks
// add that. This is the hydrate-before-dial ordering the package doc
// describes: an app that calls Connect against an unreachable server still
// gets its previously-persisted content applied to Doc, because hydration
// never depends on the network to begin with.
//
// The dial/handshake loop that reconciles with a live server is added by
// later #165 tasks; this task's Connect does not dial at all after
// hydrating.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.hydrate(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return nil
	}
}

// hydrate loads this Client's room from Store, if one was configured, and
// applies it to Doc under the remoteOrigin sentinel so persistLocalUpdate
// recognises it as already-durable and does not write it straight back.
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
// reconciles with the server on a live connection (StateSynced). It never
// closes for a Client that has not completed a sync handshake — in
// particular, this task's Connect never closes it, since it never dials.
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
// statusMu) cannot deadlock against emitStatus's own lock. This task does
// not call emitStatus from anywhere in Connect (there is no state
// transition to report yet, since Connect does not dial); it exists now so
// OnStatus is a fully working, tested part of the exported surface ahead of
// the later tasks that will call it.
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
// (once later #165 tasks add it) uses for this connection.
func (c *Client) Awareness() *awareness.Awareness {
	return c.awareness
}

// Close signals Connect to return and unregisters this Client's Doc
// persistence hook. It is safe to call more than once (only the first call
// has any effect) and safe to call concurrently with Connect.
//
// Close does not close Store: the Store is constructed and owned by the
// caller (e.g. via OpenSQLiteStore), which may want to reuse it — for
// another Client, or simply to keep it open past this Client's lifetime —
// so closing it here would take that choice away from the caller.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.unsubPersist()
	})
	return nil
}

// Stats returns a snapshot of this Client's cumulative counters. See Stats
// for what each field means; this task's Connect runs no loop, so every
// field is always zero until later #165 tasks add the code that increments
// them.
func (c *Client) Stats() Stats {
	return Stats{
		Coalesced:           c.statsCoalesced.Load(),
		AwarenessSuperseded: c.statsAwarenessSuperseded.Load(),
		HardDrops:           c.statsHardDrops.Load(),
		Dropped:             c.statsDropped.Load(),
	}
}
