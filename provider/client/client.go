package client

import (
	"context"
	"errors"
	"fmt"
	"log"
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

	// StorePath, if set, tells New to open its OWN SQLite-backed LocalStore
	// at this path (via OpenSQLiteStore) and take ownership of it: Close
	// closes the store it opened, unlike a Store supplied directly (see
	// Store's own doc — a caller-supplied Store is never closed by Close,
	// because the caller retains the handle and may want to reuse it).
	// Mutually exclusive with Store; New rejects setting both, since there
	// would be no single answer for which one Close should own.
	//
	// This exists so an embedder that has no reason to hold onto the
	// *SQLiteStore handle itself (the common case: open a local database,
	// hand it to a Client, forget about it) gets correct Close-time cleanup
	// for free instead of having to track that ownership itself. It also
	// gives later #165 work (the gomobile binding) a one-field way to say
	// "just open a local database for me at this path" without duplicating
	// this ownership bookkeeping at that layer too.
	StorePath string

	// Token is the Hocuspocus in-band auth token (mirrors
	// provider/websocket's OnTokenAuth, #104). When set, the dial loop sends
	// it as an Auth (tag 2) Token sub-message on every connection, before
	// the sync handshake (see loop.go's runLoop and wire.go's
	// encodeAuthToken, #165 Task 9). A server that rejects it (a
	// PermissionDenied reply) makes Connect return ErrAuthRejected — a
	// TERMINAL failure, not one the reconnect loop retries; see that
	// sentinel's doc. Left at its zero value (the default), nothing changes
	// from the plain y-websocket flow: no Auth frame is ever sent. Use
	// Header for HTTP-level credentials instead of, or in addition to, this.
	//
	// # NOT a confidentiality gate
	//
	// A wrong Token does not stop the room's content from reaching this
	// Client first. ygo's own server pushes SyncStep1 + a full SyncStep2 +
	// Awareness the moment a connection is accepted, before it has read
	// anything the client sent — Token included (see
	// provider/websocket.Server's OnTokenAuth doc, verbatim: "the initial
	// sync is served before any PermissionDenied, so deployments that must
	// withhold document contents from unauthenticated clients must reject
	// them at the boundary via AuthFunc/Authorize"). Concretely: this
	// Client's Doc can be fully populated with the room's real content, and
	// Synced() can already be closed, before — or even without ever —
	// learning that Token was rejected; see Synced's own doc for the
	// consequence that has for a caller. If withholding document content
	// from an unauthenticated caller matters for a deployment, Token is not
	// what does that; reject the connection at the HTTP boundary instead,
	// with provider/websocket.Server's AuthFunc or Authorize, before the
	// upgrade ever completes.
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

// ErrAuthRejected is returned by Connect — and set as Err on the final
// StateDisconnected status that precedes that return — when Options.Token
// was rejected by the server's Hocuspocus in-band auth (provider/websocket's
// #104 OnTokenAuth hook returning a non-nil error, surfaced to this client as
// a PermissionDenied reply; see loop.go's handleFrame wireMsgAuth case).
//
// Unlike every other connection failure runReconnectLoop (loop.go) handles,
// this one is TERMINAL: retrying with the same rejected token can never
// succeed — the server will keep rejecting it — so treating it like an
// ordinary transient failure would turn a bad credential into an indefinite
// hammering of the server instead of a prompt, actionable error. See
// runReconnectLoop's doc for exactly where this is checked, ahead of the
// backoff sleep, so a rejection is reported and returned on the FIRST
// attempt rather than after however many cycles it would have taken a
// caller to notice and cancel ctx themselves (#165 Task 9, #104).
//
// runLoop wraps this with additional context via fmt.Errorf's %w verb, so
// callers must use errors.Is(err, ErrAuthRejected) rather than comparing the
// returned error directly.
var ErrAuthRejected = errors.New("ygo/client: auth rejected by server")

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
	//
	// This is NOT the same claim as "and Options.Token, if set, was
	// accepted." ygo's server serves a room's full content unconditionally,
	// before it has even read a client's Token (see Options.Token's own doc,
	// "NOT a confidentiality gate"), so StateSynced can fire — and Synced()
	// can already be closed — on a connection whose token is later rejected.
	// Do not treat this state, or a closed Synced() channel, as proof of a
	// successful auth exchange; a rejection surfaces separately, either as
	// this Client's next StateDisconnected{Err: ErrAuthRejected} or as
	// Connect's return value.
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
// subscription, mirroring provider/websocket's RelayStats both in shape and
// in doc voice: Coalesced and AwarenessSuperseded are ROUTINE — expected
// under ordinary load, never evidence of loss — while Dropped and HardDrops
// going non-zero means something was actually lost and is worth alerting on.
//
// Coalesced, AwarenessSuperseded and HardDrops are read straight off the
// outbound lane (see internal/relaylane) and describe what happened to a
// payload BEFORE it was ever handed to a socket. Dropped is this Client's
// own counter (see statsDropped) and covers everything relaylane cannot see
// — loss that happens after a payload leaves the lane, or before it is even
// a Doc update at all. The exact rule (#165 Task 10, refined by that task's
// own review — see countUndeliverable for the full rationale, including the
// two-symptoms-one-cause bug that motivated stating it this precisely):
//
//   - a local Store.StoreUpdate call that returned an error (finding 2 — see
//     onDocUpdate: the edit stays in memory and in the Doc, but is no longer
//     durable across a restart, which is exactly the kind of loss this repo
//     counts rather than silently drops; also logged, for the
//     operator-facing detail a bare counter can't carry) is ALWAYS counted;
//   - a KindSync payload (a Doc update) that leaves the outbound lane
//     without reaching the wire is counted ONLY when Options.Store is nil.
//     With a Store configured, it is NEVER counted, at ANY point in this
//     Client's lifecycle including inside Close's own drain — the Store
//     already durably holds it (onDocUpdate writes there before ever
//     queueing), and the package's central design claim is that the next
//     hydrate+handshake delivers it from there, whether that is this
//     Client's own next reconnect or an entirely new Client after a process
//     restart. This is not evidence of loss, so it must not look like it;
//   - a KindAwareness payload that leaves the outbound lane without reaching
//     the wire is ALWAYS counted, Store or no Store — there is no store
//     equivalent for awareness state, and the sync handshake never conveys
//     it (see flushLane's "Take-before-write" doc section).
type Stats struct {
	// Coalesced counts local updates that were merged into a pending batch
	// rather than sent as a separate wire message, mirroring
	// provider/websocket's persistence-coalescing counters.
	Coalesced uint64
	// AwarenessSuperseded counts locally-set awareness states that were
	// superseded by a newer local SetLocalState call before ever being sent.
	AwarenessSuperseded uint64
	// HardDrops is read straight off this Client's single outbound lane
	// (internal/relaylane) — it is NOT about retrying anything, and nothing
	// in this package retries a wire send (an earlier version of this doc
	// claimed the opposite; #165 final whole-branch review, Important G).
	// The lane increments it only as a last resort: when a KindSync backlog
	// has grown past TWICE the lane's capacity (relaylane.DefaultCap, 64)
	// and crdt.MergeUpdatesV1 keeps failing to collapse the backlog into one
	// blob, the lane drops the oldest queued update rather than growing
	// without bound (see relaylane.Lane.collapseLocked's doc). That needs a
	// long enough offline burst to pile up more local edits than the lane
	// can coalesce, AND a merge failure — itself unexpected for well-formed
	// V1 updates — so this should stay rare in practice, but it is a real,
	// reachable mechanism, not a permanent zero.
	HardDrops uint64
	// Dropped counts updates actually lost — a failed local store write, a
	// KindAwareness payload that never reached the wire, or a KindSync
	// payload that never reached the wire AND had no Store to fall back on
	// (see this type's own doc for the exact rule, and countUndeliverable
	// for why it is framed this way rather than around connection state).
	// Should always be zero in a healthy deployment with a Store configured;
	// alert on it going non-zero the same way an operator would alert on
	// RelayStats.Dropped.
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

	// connectMu guards connectStarted and pairs it with connectWG.Add so the
	// two change together atomically from Connect's point of view — see
	// Connect and Close's "join the loop goroutine" doc for why that pairing
	// matters: Close reads connectStarted and decides whether to wait on
	// connectWG under the SAME lock, which is what closes the otherwise-real
	// race of Close reading "not started" a moment before Connect's own
	// Add(1) would have made that answer stale (a bare atomic.Bool plus a
	// separately-incremented WaitGroup cannot make this same guarantee: see
	// sync.WaitGroup's own doc — "note that calls with a positive delta that
	// start when the counter is zero must happen before a Wait").
	connectMu sync.Mutex
	// connectStarted latches when a Connect call is accepted, so a second one
	// is refused rather than starting a rival dial loop and a rival observer
	// on the same Doc. See Connect and ErrAlreadyConnected. Guarded by
	// connectMu (see its doc), NOT an atomic — the two need to change
	// together with connectWG.Add under one critical section.
	connectStarted bool
	// connectWG is held at 1 for exactly the span of an accepted Connect
	// call (Add right after connectMu grants it, Done via defer just before
	// Connect returns for any reason) — which, since Connect calls
	// runReconnectLoop/runLoop synchronously on its own goroutine, is
	// precisely the lifetime of "the loop goroutine". Close's Wait on this
	// (see Close) is what makes "join the loop goroutine before returning" a
	// real guarantee rather than a best-effort signal: no send this Client
	// makes can outlive Close once Close has returned. A retried Connect
	// after a hydration failure (see Connect's "does not latch" doc) reuses
	// the same WaitGroup safely — Add/Done pairs never overlap, since
	// connectMu allows only one accepted call to be in flight at a time.
	connectWG sync.WaitGroup

	// lane is the hand-off between the Doc observer (which runs on whichever
	// goroutine called Transact) and the loop goroutine (the socket's only
	// writer). See onDocUpdate for why the hand-off has to exist at all, and
	// internal/relaylane for its bounded, coalescing, never-blocking policy.
	// It is created in New rather than per-connection so local edits made
	// while offline queue up for the next connection instead of needing a
	// separate holding pen.
	lane *relaylane.Lane

	// compactor is o.Store re-asserted to CompactableStore once, in New,
	// rather than on every maybeCompact call — nil (the common case, e.g. a
	// nil Store or a LocalStore that doesn't implement it) makes maybeCompact
	// a single nil-check away from a full no-op. See maybeCompact.
	compactor CompactableStore
	// storeWrites counts successful Store.StoreUpdate calls since the last
	// compaction attempt (successful or not — see maybeCompact), incremented
	// from onDocUpdate on whichever goroutine that runs on (the caller's own
	// Transact goroutine for a local edit, or the loop goroutine for a
	// server-received one — see #165 Task 10 finding 3) and read/reset only
	// from the loop goroutine inside maybeCompact. The atomic is what makes
	// that increment-from-either-side, reset-from-one-side split safe
	// without a lock.
	storeWrites atomic.Uint64

	// ownedStore is non-nil exactly when Options.StorePath was used: the
	// *SQLiteStore New opened on this Client's behalf, and therefore the one
	// Store this Client's Close is responsible for closing. A Store supplied
	// directly via Options.Store is never tracked here and is never closed
	// by Close — see Options.StorePath's doc and Close's ownership rule.
	ownedStore *SQLiteStore

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
	if o.Store != nil && o.StorePath != "" {
		// See Options.StorePath's doc: closing the store is Close's job, and
		// there is no single correct answer for which one to close (or
		// whether to close both — a caller-supplied Store must never be
		// closed by this package, full stop) if both are set, so this is
		// rejected outright rather than silently preferring one.
		return nil, errors.New("client: Options.Store and Options.StorePath are mutually exclusive")
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

	// See Options.StorePath's doc: opened here (construction time, not
	// Connect) so a failure to open it is reported synchronously from New,
	// exactly like every other Options validation above, rather than
	// surfacing later from inside Connect's hydrate step.
	var owned *SQLiteStore
	if o.StorePath != "" {
		ss, err := OpenSQLiteStore(o.StorePath)
		if err != nil {
			return nil, fmt.Errorf("client: open store at %q: %w", o.StorePath, err)
		}
		o.Store = ss
		owned = ss
	}
	// Asserted once, here, rather than on every maybeCompact call — see
	// Client.compactor's own doc.
	compactor, _ := o.Store.(CompactableStore)

	c := &Client{
		opts:          o,
		room:          room,
		remoteOrigin:  &remoteOrigin{},
		hydrateOrigin: &hydrateOrigin{},
		awareness:     awareness.New(uint64(o.Doc.ClientID())),
		lane:          relaylane.New(0), // 0 = relaylane.DefaultCap
		compactor:     compactor,
		ownedStore:    owned,
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
		// #165 Task 10 finding 2: a failed local write used to be dropped
		// silently (no Stats field or Status shape fit it at the time). That
		// is exactly the class of loss RelayStats.Dropped's doc says this
		// repo counts rather than logs-and-forgets: the edit survives only
		// in memory now, not across a restart, until some later write to
		// the same room happens to succeed. Both halves matter here, not
		// just one — Stats().Dropped is what a caller with no log access
		// (e.g. the mobile binding) can act on programmatically, and the log
		// line is what an operator watching this process's logs sees with
		// the room name and the actual error attached, mirroring
		// provider/websocket's own store closure in persistence.go (log,
		// never fatal, no retry).
		if err := c.opts.Store.StoreUpdate(c.room, update); err != nil {
			log.Printf("ygo/client: StoreUpdate for room %q: %v", c.room, err)
			c.statsDropped.Add(1)
		} else {
			// Feeds Client.maybeCompact's threshold — see storeWrites' own
			// doc for why only a SUCCESSFUL write counts: a failed
			// StoreUpdate did not add a row for Compact to ever trim.
			c.storeWrites.Add(1)
		}
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
// Only THIS Client's own clientID is ever re-encoded and pushed here, never
// the Client's entire known state (EncodeUpdate(nil)) and never any OTHER id
// evt happens to carry (see the filtering below and its own doc for why that
// is a hard rule, not merely "every real call this package makes only
// touches one id" — an earlier version of this comment claimed exactly that,
// and #165's final whole-branch review found it false: a caller is entitled
// to drive c.Awareness() directly, per that method's own doc, and
// Awareness.RemoveExpired/StartAutoExpiry do so with a nil Origin — never
// c.remoteOrigin — and a Removed set that is always some OTHER peer's
// clientID, never this one's).
//
// # One narrow exception to "never re-send a remoteOrigin event"
//
// Awareness.ApplyUpdate has its own self-state protection (#73 vector C1,
// awareness/awareness.go): if the update this Client just applied under
// c.remoteOrigin contained a null entry for OUR OWN clientID, ApplyUpdate
// does not honor it — it bumps a.clock past the incoming value and re-emits
// our current (still-active) state, in the SAME ApplyUpdate call, so our
// in-memory Awareness never actually looks removed. But that correction is
// reported back to us with Origin still set to whatever ApplyUpdate's
// caller passed in — c.remoteOrigin — because from ApplyUpdate's point of
// view this update DID arrive from the network; it has no way to know the
// caller is about to reinterpret it. Left fully suppressed, that correction
// would never reach the wire: every OTHER peer that already accepted the
// bad null entry (a legitimate accept under ApplyUpdate's own clock gate —
// see loop.go's handshake-completion comment for a concrete way such a
// belated, wrongly-targeted removal can arise) keeps believing we are gone
// until this Client's next periodic Heartbeat, up to a full PingInterval
// later by default.
//
// So: when the remoteOrigin event's Updated set contains our own clientID
// (self-state protection's only possible outcome is to add it there — see
// ApplyUpdate's source; never Added, never Removed) AND we still have an
// active local state to reassert (GetLocalState() != nil — if it's nil,
// ApplyUpdate found nothing worth correcting and there is nothing to
// re-announce), forward ONLY that one clientID. This must stay exactly this
// narrow: forwarding the rest of a remoteOrigin event's IDs here would
// reopen the general echo storm this whole suppression exists to prevent.
func (c *Client) onAwarenessUpdate(evt awareness.UpdateEvent) {
	if evt.Origin == c.remoteOrigin {
		if !containsClientID(evt.Updated, c.awareness.ClientID()) || c.awareness.GetLocalState() == nil {
			return
		}
		c.lane.Push(cluster.KindAwareness, c.awareness.EncodeUpdate([]uint64{c.awareness.ClientID()}))
		return
	}
	// Filter to THIS Client's own clientID — see this method's doc for why
	// that is a hard rule now, not an incidental fact about the only calls
	// this package itself makes. A no-op for SetLocalState/Heartbeat (they
	// only ever touch the local id, so it is always present when anything
	// else is); load-bearing for a caller driving c.Awareness() directly,
	// e.g. RemoveExpired/StartAutoExpiry, whose Removed set is always some
	// OTHER peer's clientID and must never reach the wire from here (#165
	// final whole-branch review, Important C — see
	// TestClient_Awareness_CallerDrivenExpiryDoesNotRebroadcastOtherPeers).
	localID := c.awareness.ClientID()
	if !containsClientID(evt.Added, localID) &&
		!containsClientID(evt.Updated, localID) &&
		!containsClientID(evt.Removed, localID) {
		return
	}
	c.lane.Push(cluster.KindAwareness, c.awareness.EncodeUpdate([]uint64{localID}))
}

// containsClientID reports whether ids contains target. A small linear
// helper rather than a set: onAwarenessUpdate's ids lists are always tiny
// (one entry in every real call this package makes; see that method's doc),
// so there is nothing for a map to buy here.
func containsClientID(ids []uint64, target uint64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
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
// was unhappy about is resolved. It is NOT an exception to the OnStatus
// contract, though: like every other way Connect can end (the terminal
// ErrAuthRejected path, and the ordinary "stopped on purpose" bookend), a
// hydration failure reports itself via OnStatus — StateDisconnected carrying
// the error — before Connect returns it. This was a real gap until #165
// Task 11's review caught it (a caller driving UI purely from OnStatus, such
// as the mobile SyncClient binding, saw nothing at all on a corrupt local
// store); see the emitStatus call at the top of the hydrate-failure branch
// below.
func (c *Client) Connect(ctx context.Context) error {
	c.connectMu.Lock()
	if c.connectStarted {
		c.connectMu.Unlock()
		return ErrAlreadyConnected
	}
	c.connectStarted = true
	// Add happens in the SAME critical section as the latch flip — see
	// connectWG's doc for why that pairing, not just the Add itself, is what
	// makes Close's Wait race-free against a Connect call that has only just
	// been accepted.
	c.connectWG.Add(1)
	c.connectMu.Unlock()
	defer c.connectWG.Done()

	if err := c.hydrate(); err != nil {
		// Emitted HERE, before releasing the guard below, and therefore
		// still inside the window connectWG.Add(1) (above) / Done (deferred)
		// covers — i.e. before Close's connectWG.Wait() can possibly return,
		// exactly like every other status this Client ever emits. #165 Task
		// 11's review caught an earlier mobile-binding workaround that
		// emitted this same information from OUTSIDE that window (after
		// Connect had already returned to its caller's goroutine), which
		// could let Close's Wait return before the caller's status handler
		// ever ran. Emitting from the one place this error actually occurs
		// removes that whole class of ordering bug rather than requiring
		// every caller to route around it themselves.
		//
		// The ORDER of this call relative to the connectStarted reset just
		// below is itself load-bearing, not incidental — this is precisely
		// what the final whole-branch review's Important A finding was
		// about. Close decides whether to wait on connectWG by reading
		// connectStarted under connectMu (see Close's doc); it does not
		// wait unconditionally. Resetting connectStarted to false BEFORE
		// this emitStatus call would let a Close running concurrently on
		// another goroutine observe "not started" — under the very same
		// lock — and skip connectWG.Wait() entirely, which would let Close
		// return while this emitStatus call (and therefore whatever a
		// subscriber's callback is doing) is still running: exactly the
		// violation the previous paragraph's "before Close's
		// connectWG.Wait() can possibly return" claim was supposed to rule
		// out, and didn't, because the guard had already been released one
		// statement too early. Emitting first closes that window: a
		// concurrent Close reading connectStarted at ANY point before this
		// call returns is guaranteed to still see true, and will therefore
		// wait — see TestClient_Connect_HydrateFailureKeepsCloseWaitingForStatus
		// for the deterministic proof (a callback held open by the test,
		// with a concurrent Close asserted not to return while it is).
		c.emitStatus(Status{State: StateDisconnected, Err: err})
		// Nothing has been started, so release the guard: unlike every later
		// failure, this one leaves the Client exactly as New returned it.
		// Safe to do only NOW, after the emission above has fully returned.
		c.connectMu.Lock()
		c.connectStarted = false
		c.connectMu.Unlock()
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

	// runReconnectLoop returns nil in the ordinary case — it only stops
	// because loopCtx is done (see its own doc) — with every connection
	// failure along the way already reported through OnStatus as it
	// happened, so there is nothing left to inspect once it returns nil.
	// The final, unconditional emission below is the bookend
	// StateDisconnected's own doc describes ("after Close") — it is
	// deliberately Err: nil even if the very last attempt before the stop
	// had failed, because THAT failure was already reported with its real
	// error by runReconnectLoop; this one specifically means "and now the
	// client is stopped, on purpose."
	//
	// A non-nil return is the one exception (#165 Task 9): a terminal
	// failure — currently only ErrAuthRejected — that runReconnectLoop
	// deliberately did NOT retry. It has already reported that failure via
	// OnStatus too (see its own doc), so this just surfaces the same error
	// through Connect's return value instead of falling through to the
	// "stopped on purpose" bookend below, which would misreport a rejected
	// token as a clean stop.
	if err := c.runReconnectLoop(loopCtx); err != nil {
		return err
	}
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
//
// # This channel closing is not proof Options.Token was accepted
//
// If Options.Token is set, do not treat a closed Synced() as "and therefore
// authenticated". ygo's server serves a room's full SyncStep1/SyncStep2/
// Awareness state unconditionally, before it has read the client's Token at
// all (see Options.Token's "NOT a confidentiality gate" doc) — so this
// channel can close, with the Doc already carrying the room's real content,
// on a connection whose token is rejected moments later. A caller that needs
// to know the token was actually accepted has to watch for the ABSENCE of a
// subsequent StateDisconnected{Err: ErrAuthRejected} (via OnStatus) or for
// Connect returning without ErrAuthRejected — Synced() alone does not carry
// that information, by design (it answers "does the Doc have the server's
// state", not "was this connection authorized"). A deployment that needs to
// withhold content pending authorization should reject the connection at the
// HTTP boundary instead (provider/websocket.Server's AuthFunc/Authorize),
// before this Client ever gets far enough to sync anything.
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
//
// # fn must never call Close
//
// Freedom from statusMu (above) does not extend to this Client's OTHER
// locks. Every Status is delivered synchronously, on the loop goroutine
// Connect owns (see emitStatus's own doc: "every call site is on the loop
// goroutine or on Connect's own goroutine around it"), and Close's very
// first step is to join exactly that goroutine (connectWG.Wait — see
// Close's doc) before it does anything else. A fn that calls Close is
// therefore that same goroutine waiting on itself: a permanent deadlock,
// not merely a slow callback that stalls this Client's sync the way
// emitStatus's doc already warns a blocking fn does. This is a real
// hazard, not a theoretical one — "disconnect and give up" is an obvious
// thing for an OnStatus subscriber watching for StateDisconnected to want
// to do. A caller embedding this package directly and needing that pattern
// must hand the Status off (e.g. to a channel, and call Close from a
// different goroutine reading it) rather than calling Close from inside fn.
// See mobile.SyncClient's SyncStatusObserver for a worked example of that
// hand-off — it exists specifically because a platform OnStatus
// implementation cannot be expected to know about this rule at all.
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
//
// # Driving expiry on the returned instance is safe, but never re-broadcasts anyone else
//
// A caller is entitled to call RemoveExpired or StartAutoExpiry directly on
// the returned *awareness.Awareness — that is the pattern the awareness
// package's own doc recommends, and this Client does not require it to be
// driven any particular way. Doing so only ever prunes THIS Client's local
// view of other peers; onAwarenessUpdate (registered by New) never forwards
// a resulting removal for anyone but this Client's own clientID onto the
// wire, no matter whose ids RemoveExpired's UpdateEvent actually names (#165
// final whole-branch review, Important C — RemoveExpired never self-expires
// the local client, so its Removed set is always some OTHER peer's id, and
// re-broadcasting that would let a real server evict a peer that is still
// perfectly present). Concretely: calling RemoveExpired/StartAutoExpiry here
// affects only what GetStates returns locally, never what this Client sends.
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

// Close signals Connect to tear down its connection and return, then blocks
// until that has genuinely finished, following the same discipline #202
// (provider/websocket's Shutdown, see cluster.go) established for exactly
// this shape of problem: durability first, then a bounded best-effort
// network flush that COUNTS what it could not deliver rather than silently
// discarding it, then teardown. It is safe to call more than once (only the
// first call has any effect) and safe to call concurrently with Connect.
//
// # Never call this from inside an OnStatus callback
//
// See OnStatus's own "fn must never call Close" doc for the full argument.
// In short: step 1 below joins the loop goroutine that every OnStatus
// callback runs on, so a callback that calls Close is that goroutine
// waiting on itself — a permanent deadlock, not a slow or merely-blocked
// Close. mobile.SyncClient's SyncStatusObserver is built specifically to
// let platform code close from its own status handler safely; embedding
// this package directly does not get that protection and must not call
// Close from fn.
//
// # Ordering, and why
//
//  1. Signal (close c.closed) and wait for the current Connect call, if any,
//     to fully return — see connectWG's doc. Since Connect calls
//     runReconnectLoop/runLoop synchronously on its own goroutine, this IS
//     "join the loop goroutine": by the time this wait returns, no goroutine
//     this Client owns can still be reading frames, still be inside
//     runLoop's select, or still hold the socket open. Whatever draining
//     that loop could do on its way out (see loop.go's ctx.Done() case and
//     runReconnectLoop's own two "give up" points) has already happened —
//     this call does not additionally drain a socket itself, because doing
//     so from a second goroutine while the loop goroutine might still be
//     mid-write would violate the single-writer invariant loop.go documents
//     on session. This is also why store writes are "done" by the time this
//     returns: every store write this Client ever makes happens inside
//     onDocUpdate, called synchronously from Transact — nothing here is
//     asynchronous or buffered the way provider/websocket's persistence
//     worker is, so there is no separate flush step to wait for beyond "no
//     goroutine that could still call onDocUpdate is running."
//  2. Unsubscribe the Doc and Awareness observers, so a Doc/Awareness pair
//     that outlives this Client stops writing to the Store and stops
//     queueing anything onto a lane nothing will ever drain again. Placed
//     AFTER the join (so that any server-received update the loop was still
//     applying in its very last moments is still stored — see onDocUpdate's
//     table: a remoteOrigin update must be persisted, and removing the
//     observer before the join would have silently skipped that for
//     whatever arrived in the final frame(s) of the last connection) but
//     BEFORE the catch-all drain in step 3 (#165 Task 10 review, Important
//     2 — this was the other way around originally, and the two orderings
//     are not each other's mirror image the way that might suggest: draining
//     before unsubscribing leaves a real window where an application
//     goroutine's own doc.Transact or Awareness().SetLocalState call — nothing
//     stops an app from editing its own Doc while Close is tearing down —
//     lands a payload on the lane via the still-live observer, strictly
//     AFTER the one and only drain this method ever performs, meaning
//     nothing is left to ever collect it: permanently stuck, uncounted, and
//     silently unreachable once Close returns. Unsubscribing first closes
//     that window structurally: nothing can land on the lane through the
//     observer path anymore by the time step 3 runs, so whatever step 3
//     finds is everything there will ever be to find).
//  3. Catch-all: count whatever is STILL on the outbound lane as Dropped
//     (see dropLaneRemainder). This is a no-op in the ordinary case — the
//     loop's own teardown (step 1) already drained or counted everything —
//     and only does real work for the "no loop ever ran to drain anything"
//     case: Connect was never called, or every attempt failed during
//     hydration (see Connect's "does not latch" doc), so nothing else in
//     this Client will ever get a chance to touch the lane again.
//  4. Close the store, but ONLY if this Client opened it itself (Options.
//     StorePath, tracked via ownedStore — see Options.StorePath's doc and
//     ownedStore's own). A Store supplied directly via Options.Store
//     belongs to the caller, which may want to reuse it for another Client
//     or simply keep it open past this one's lifetime; closing it here
//     would take that choice away.
//
// Close's own return value surfaces ownedStore.Close's error, if any;
// Connect's return value (observed separately, via whatever channel the
// caller used to receive it) is unaffected by this method.
func (c *Client) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		close(c.closed)
		// Read connectStarted under connectMu — the SAME lock Connect's
		// Add(1) is taken under — rather than calling connectWG.Wait()
		// unconditionally. This is not an optimisation: sync.WaitGroup's own
		// doc is explicit that "calls with a positive delta that start when
		// the counter is zero must happen before a Wait" — i.e. Wait needs
		// an actual synchronisation edge to the matching Add, not merely an
		// Add that has, in physical time, already happened. Acquiring (and
		// releasing) connectMu here is what supplies that edge: Connect's
		// Unlock after Add(1) synchronises-with this Lock, so if
		// connectStarted reads true here, this goroutine is guaranteed to
		// observe that Add — a bare, lockless Wait() had no such guarantee
		// and raced under -race even though the accepted call in this
		// package's own tests always sequenced correctly in practice.
		c.connectMu.Lock()
		started := c.connectStarted
		c.connectMu.Unlock()
		if started {
			c.connectWG.Wait()
		}
		c.unsubObserver()
		c.unsubAwareness()
		// closePreDrainHook is a test-only seam (see close_test.go's
		// withClosePreDrainHook) fired at exactly the point step 2's doc
		// above describes: observers already gone, catch-all drain not yet
		// run. Nil (a no-op) outside a test. It exists because the ordering
		// bug this seam guards against (Important 2 above) has no other
		// deterministic way to prove fixed: a real race between an
		// application goroutine and Close would need to land inside a
		// window measured in nanoseconds to reproduce on demand, so a test
		// that actually depends on winning that race would be exactly the
		// kind of flake this package already has a documented history of
		// (see close_test.go's own doc). Fires unconditionally, not just
		// under a build tag, mirroring flushWriteTimeout/closeDrainTimeout's
		// own "package-level indirection purely for test determinism"
		// precedent — the cost of one no-op function call on every Close is
		// negligible next to what it buys in test determinism.
		closePreDrainHook()
		c.dropLaneRemainder()
		if c.ownedStore != nil {
			closeErr = c.ownedStore.Close()
		}
	})
	return closeErr
}

// closePreDrainHook is Close's test-only synchronisation seam; see its one
// call site's doc for what it is for and why it exists. Always nil (a no-op)
// in production.
var closePreDrainHook = func() {}

// countUndeliverable increments Stats().Dropped for a payload that has just
// left the outbound lane without ever reaching the wire — UNLESS something
// durable will still deliver it regardless of what this Client does next.
// See Stats' own doc for the full rationale; the rule itself, applied
// exactly as stated wherever a payload leaves the lane unwritten (flushLane's
// failed write and its give-up drain, and dropLaneRemainder):
//
//   - a KindSync payload (isSync) with Options.Store configured is NEVER
//     counted. onDocUpdate already wrote it there, synchronously, before it
//     was ever queued (see onDocUpdate) — the package's central design claim
//     is that the next hydrate+handshake, whether this Client's own next
//     reconnect or an entirely new Client after a process restart, delivers
//     it from the Store, so this is not a loss, only a delay.
//   - a KindSync payload with Options.Store nil IS counted. Nothing durable
//     backs it once it leaves the lane; the in-memory Doc alone survives
//     reconnects WITHIN this process but not the process exiting, and is not
//     a guarantee this method can rely on regardless.
//   - a KindAwareness payload IS counted, Store or no Store: there is no
//     store equivalent for awareness state, and the sync handshake never
//     conveys it (see flushLane's own "Take-before-write" doc section).
//
// This is deliberately a STATIC rule — evaluated purely from (isSync,
// whether Options.Store is non-nil), never from ctx or from whether Close
// has begun. An earlier version of this accounting (#165 Task 10's original
// review) decided "is this lost?" from connection/ctx state instead, trying
// to predict whether a future connection would still arrive in time to
// redeliver a payload. That framing produced two symptoms from one root
// cause: it under-counted a KindSync payload whose write failed while ctx
// was still technically live but Close ended up intervening before any
// reconnect actually completed (a real, reproducible race — the failure and
// Close's own teardown could interleave in either order), and it
// over-counted a Store-backed KindSync payload purely because it happened to
// still be queued when a loop stopped running, even though the Store already
// held it durably and nothing was actually at risk. Neither symptom is
// possible once the decision depends only on facts knowable immediately, at
// the moment a payload leaves the lane, instead of on what happens next.
func (c *Client) countUndeliverable(isSync bool) {
	if isSync && c.opts.Store != nil {
		return
	}
	c.statsDropped.Add(1)
}

// dropLaneRemainder takes every payload still queued on the outbound lane
// and, per countUndeliverable's rule, counts whichever of them are actually
// lost in Stats().Dropped — discarding all of them either way, never
// leaving one queued (nothing will ever drain it again once this runs) and
// never letting a genuinely lost one vanish uncounted, which is the #202
// invariant this whole task exists to enforce.
//
// Safe to call when the lane is already empty (the ordinary case: whatever
// loop ran already drained or counted everything on its own way out — see
// loop.go's ctx.Done() case and runReconnectLoop's two "give up" points,
// both of which call this too for exactly the runs that never reach a live
// session to drain onto). Every call site runs on a goroutine that cannot be
// racing another Take on this same lane: Close's call happens only after
// connectWG.Wait() has confirmed no Connect-owned goroutine is still
// running AND after the observers have been unsubscribed (see Close's own
// doc, Important 2), and the loop.go call sites all run ON that one
// goroutine.
func (c *Client) dropLaneRemainder() {
	for {
		if _, ok := c.lane.TakeSync(); ok {
			c.countUndeliverable(true)
			continue
		}
		if _, ok := c.lane.TakeAwareness(); ok {
			c.countUndeliverable(false)
			continue
		}
		return
	}
}

// maybeCompact asks c.compactor to collapse this Client's room's stored
// update log once c.storeWrites has accumulated past Options.CompactEvery
// since the last attempt, then resets the counter — mirroring
// provider/websocket's startPersistenceWorker.onFlushed/maybeCompact
// (persistence.go): contained by recover, an error is logged and never
// fatal, and the counter resets REGARDLESS of the outcome, so a store that
// fails compaction once is retried after another CompactEvery writes rather
// than wedging this Client's sync loop retrying the same failure forever.
//
// A no-op when c.compactor is nil (Store is nil, or does not implement
// CompactableStore — see CompactableStore's own doc) or the threshold has
// not been reached, which keeps this cheap to call unconditionally.
//
// Called from runLoop's own select loop, between messages (see runLoop):
// this Client serves exactly one room, unlike provider/websocket's
// per-room worker goroutines, so a brief pause here cannot head-of-line
// block any OTHER room the way it would server-side — there is no other
// room. One consequence worth stating plainly: compaction only ever runs
// while a connection is live and cycling through its select loop. A device
// that stays offline for its entire lifetime accumulates an uncompacted log
// for as long as that lasts; this is an accepted simplification (#165 Task
// 10 YAGNI), not an oversight — the alternative (a free-standing ticker
// unrelated to the loop) would need its own goroutine and its own
// coordination with Close for a case this package's design does not
// otherwise need to solve.
func (c *Client) maybeCompact(ctx context.Context) {
	if c.compactor == nil {
		return
	}
	if c.storeWrites.Load() < uint64(c.opts.CompactEvery) {
		return
	}
	c.storeWrites.Store(0)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ygo/client: Compact panic for room %q: %v", c.room, r)
		}
	}()
	if err := c.compactor.Compact(ctx, c.room); err != nil {
		log.Printf("ygo/client: Compact for room %q: %v", c.room, err)
	}
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
