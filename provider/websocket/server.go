package websocket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gws "github.com/gorilla/websocket"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/roomname"
	ygsync "github.com/reearth/ygo/sync"
)

// safeHook invokes fn under a deferred recover so a panicking user hook
// cannot crash the calling goroutine. The panic is logged with the stack
// at Error level and the recovered value attached to the log entry; the
// caller continues as if the hook completed normally. Use for every
// user-supplied callback the server invokes (lifecycle hooks, stateless,
// inject, etc.) so an embedding application bug never takes down a
// connection-handling or room-disposal goroutine.
func (s *Server) safeHook(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log().Error("hook panicked; recovered",
				"hook", name,
				"panic", r,
				"stack", string(debug.Stack()))
		}
	}()
	fn()
}

// writeTimeout is applied to every individual WebSocket write. A peer that
// stops reading will be detected and disconnected within this window, preventing
// a slow-reader from blocking the broadcast loop for all other peers.
const writeTimeout = 10 * time.Second

// Outer message type codes. Tags 0-3 are the y-protocols / y-websocket
// core; tags 4-10 are the Hocuspocus protocol extensions ygo accepts
// from clients (#55). See the StatelessHook godoc for the application-
// level contract on Stateless and BroadcastStateless.
//
// NOTE: Hocuspocus's framing prepends a VarString(docName) to every
// frame so a single WebSocket connection can multiplex multiple
// documents. ygo's framing is the y-websocket layout (tag + payload),
// one document per connection. So ygo accepts the Hocuspocus message
// TYPES on the existing y-websocket framing but does not implement
// multi-document multiplexing.
const (
	msgSync           = uint64(0)
	msgAwareness      = uint64(1)
	msgAuth           = uint64(2) // y-websocket auth; silently ignored
	msgQueryAwareness = uint64(3)

	// Hocuspocus extensions, accepted on the y-websocket framing.
	msgSyncReply          = uint64(4)  // SyncStep2 / Update that must NOT trigger a SyncStep1 reply
	msgStateless          = uint64(5)  // arbitrary VarString payload, surfaced via Server.OnStateless
	msgBroadcastStateless = uint64(6)  // VarString payload, fanned out to other peers as msgStateless
	msgClose              = uint64(7)  // peer-requested graceful close (optional VarString reason)
	msgSyncStatus         = uint64(8)  // server→client update-applied ack; if a client sends it, no-op consume
	msgPing               = uint64(9)  // liveness check; replies with msgPong
	msgPong               = uint64(10) // liveness reply to a server-sent Ping; no-op
)

// maxWSMessageBytes is the maximum size of a single WebSocket frame accepted
// by the server. Frames larger than this are rejected before being buffered,
// preventing OOM from a single crafted large message.
const maxWSMessageBytes int64 = 64 << 20 // 64 MiB

// maxMessageBytes returns the configured per-message cap or the default.
func (s *Server) maxMessageBytes() int64 {
	if s.MaxMessageBytes > 0 {
		return s.MaxMessageBytes
	}
	return maxWSMessageBytes
}

const defaultHandshakeTimeout = 30 * time.Second

// handshakeTimeout returns the configured first-read deadline or the default.
func (s *Server) handshakeTimeout() time.Duration {
	if s.HandshakeTimeout > 0 {
		return s.HandshakeTimeout
	}
	return defaultHandshakeTimeout
}

// log returns the configured logger or slog.Default().
func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// peerWriteQueueSize returns the configured per-peer write queue capacity or
// the default.
func (s *Server) peerWriteQueueSize() int {
	if s.PeerWriteQueueSize > 0 {
		return s.PeerWriteQueueSize
	}
	return defaultPeerWriteQueueSize
}

// maxAwarenessClientsPerPeer caps the number of awareness clientIDs one peer
// may claim ownership of. Without this cap an attacker can send an awareness
// update listing 1,000,000 clientIDs and cause an OOM when handleDisconnect
// builds the removal slice (N-H4).
const maxAwarenessClientsPerPeer = 10_000

// defaultPeerWriteQueueSize is the default capacity of each peer's broadcast
// write channel when PeerWriteQueueSize is not set.
const defaultPeerWriteQueueSize = 256

// PersistenceAdapter is implemented by storage backends that want to persist
// room state across server restarts. It is called on every committed update so
// implementations should be efficient (e.g. append-only log rather than full
// re-encode on every write).
type PersistenceAdapter interface {
	// LoadDoc returns the full binary V1 update representing stored state for
	// the room, or (nil, nil) if no state exists yet.
	LoadDoc(room string) ([]byte, error)
	// StoreUpdate is called with each incremental V1 update produced by a
	// transaction in the room. The adapter is responsible for merging or
	// appending updates as appropriate for its storage model.
	StoreUpdate(room string, update []byte) error
}

// PersistenceAdapterContext is an optional extension to PersistenceAdapter.
// Adapters that implement this interface receive a context that is cancelled
// when the server begins shutdown, letting the adapter abort in-flight writes
// (network calls, DB queries, etc.) rather than blocking Shutdown indefinitely.
//
// The persistence worker checks for this interface at runtime via a type
// assertion. Adapters that implement only PersistenceAdapter remain fully
// supported — the worker falls back to StoreUpdate when StoreUpdateContext
// is unavailable.
//
// Pattern mirrors io.WriterTo / http.CloseNotifier and the database/sql/driver
// Queryer / QueryerContext family in the standard library.
type PersistenceAdapterContext interface {
	// StoreUpdateContext is the context-aware variant of StoreUpdate. It is
	// called with a ctx that is cancelled when Server.Shutdown begins. The
	// adapter should respect cancellation (e.g., abort the network call or
	// DB transaction) and return ctx.Err() when ctx is done.
	StoreUpdateContext(ctx context.Context, room string, update []byte) error
}

// MemoryPersistence is a thread-safe in-memory PersistenceAdapter that merges
// all updates into a single V1 snapshot per room. It is the default adapter
// used when no external persistence is configured and is primarily useful in
// tests and single-process deployments.
type MemoryPersistence struct {
	mu   sync.RWMutex
	docs map[string][]byte // room → merged V1 update
}

// NewMemoryPersistence returns an empty MemoryPersistence.
func NewMemoryPersistence() *MemoryPersistence {
	return &MemoryPersistence{docs: make(map[string][]byte)}
}

// LoadDoc returns the merged V1 update for room, or nil if none exists.
func (m *MemoryPersistence) LoadDoc(room string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.docs[room], nil
}

// StoreUpdate merges update into the stored snapshot for room.
func (m *MemoryPersistence) StoreUpdate(room string, update []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.docs[room]
	if len(existing) == 0 {
		m.docs[room] = update
		return nil
	}
	merged, err := crdt.MergeUpdatesV1(existing, update)
	if err != nil {
		return err
	}
	m.docs[room] = merged
	return nil
}

// room holds the shared document and awareness state for one named room.
type room struct {
	mu        sync.Mutex
	doc       *crdt.Doc
	awareness *awareness.Awareness
	peers     map[*peer]struct{}

	// peerSem enforces MaxPeersPerRoom as a hard cap. Initialised at room
	// creation time. nil when MaxPeersPerRoom == 0 (unlimited).
	peerSem *semaphore.Weighted

	// Persistence write queue. nil when no PersistenceAdapter is configured.
	persistCh   chan []byte   // buffered channel for serialised writes
	persistStop chan struct{} // closed to signal goroutine to drain and exit
	persistDone chan struct{} // closed when persistence goroutine exits

	// relayUnsub holds the doc.OnUpdate / awareness.OnChange unsubscribe
	// functions registered when a Relay is attached. nil when no relay. Called
	// once when the room is evicted so the relay observers don't leak. Guarded
	// by the room's mu via registerRelayObservers / unregisterRelayObservers.
	relayUnsub []func()
}

// Server is a net/http-compatible WebSocket handler.
// Each distinct room name maps to an independent Yjs document.
type Server struct {
	upgrader    gws.Upgrader
	rmu         sync.RWMutex
	rooms       map[string]*room
	persistence PersistenceAdapter

	// relay, when non-nil, mirrors local doc/awareness changes to other server
	// nodes and applies inbound changes. Set once via AttachRelay. relayCtx /
	// relayCancel govern the relay's delivery lifetime; cancelled on Shutdown.
	// relaySentinel is the origin stamped on relay-injected changes so the
	// per-room observers can drop echoes (pointer-identity guard).
	//
	// relayMu guards the attach handshake: AttachRelay only commits s.relay
	// after relay.Start succeeds, so a Start failure leaves the server
	// unattached and the call is retryable (no sync.Once latching a partial
	// attach). relayOut is the bounded outbound queue the CRDT observers
	// enqueue onto; a dedicated worker drains it and drives relay.Publish so
	// the commit path never blocks on a slow relay. See cluster.go.
	relayMu       sync.Mutex
	relay         clusterRelay
	relaySentinel any
	relayCtx      context.Context
	relayCancel   context.CancelFunc
	relayOut      chan relayOutbound
	relayDropped  atomic.Uint64

	shutdownOnce sync.Once
	shutdownCh   chan struct{} // closed by Shutdown

	// AuthFunc, if non-nil, is called before upgrading each incoming WebSocket
	// connection. Return false to reject the connection; the server responds
	// with 401 Unauthorized. Use this hook for token validation, session checks,
	// or IP allow-lists. If nil, all connections are accepted.
	AuthFunc func(r *http.Request) bool

	// AllowedOrigins is the list of origins permitted to open WebSocket
	// connections (C2 — CORS). Each entry is a full origin string, e.g.
	// "https://example.com". An entry may contain "*" wildcards, each matching
	// any run of characters: "https://*.example.com" matches any subdomain and
	// "https://pr-*---web-*.run.app" matches preview hosts. A bare "*" allows any
	// origin. Matching is case-insensitive.
	//
	// If the slice is empty the server falls back to a same-origin check:
	// the request Origin header must match the HTTP Host header. Non-browser
	// clients that omit the Origin header are always permitted.
	//
	// Security warning: setting AllowedOrigins to "*" disables same-origin
	// protection and enables Cross-Site WebSocket Hijacking (CSWSH) — a
	// malicious page that the user visits can open a WebSocket to this
	// server and act as that user if authentication is carried by a session
	// cookie. Use "*" only when AuthFunc validates tokens carried explicitly
	// (bearer tokens in the WebSocket subprotocol or a query parameter), not
	// when relying on cookie-based auth. See SECURITY.md.
	AllowedOrigins []string

	// MaxConnections is the server-wide cap on simultaneous WebSocket peers.
	// Upgrade requests that would exceed this limit are rejected with 503.
	// Zero (the default) means unlimited (N-H5).
	MaxConnections int

	// MaxPeersPerRoom is the per-room cap on simultaneous WebSocket peers.
	// Upgrade requests that would exceed this limit are rejected with 503.
	// Zero (the default) means unlimited (N-H5).
	MaxPeersPerRoom int

	// OnInject, if non-nil, is called before every server-side write
	// (BroadcastUpdate or Apply). Return a non-nil error to refuse the
	// operation; the error is wrapped and returned to the caller.
	// For BroadcastUpdate, InjectInfo.UpdateSize is len(update); for
	// Apply it is 0 (the delta has not yet been produced).
	OnInject InjectHook

	// OnStateless, if non-nil, is called when a peer sends a Hocuspocus
	// Stateless (tag 5) or BroadcastStateless (tag 6) message. The hook
	// is purely informational — for BroadcastStateless the server has
	// already fanned the payload out to other peers in the room by the
	// time the hook fires. Use this to surface out-of-band signals
	// (Tiptap comments, custom presence metadata, application heartbeats)
	// to the embedding application.
	OnStateless StatelessHook

	// OnLoadDocument, if non-nil, is called once per room immediately
	// after the document has been bootstrapped from the PersistenceAdapter
	// (or freshly constructed when no adapter is configured) but before
	// any peer can interact with it. Returning a non-nil error fails room
	// creation: peer upgrades / Apply / BroadcastUpdate against the room
	// receive that error wrapped as a room-load failure. Use this to wire
	// in a custom resolver, decrypt-at-rest, schema-migration check, or
	// any other one-time per-room setup. (#60)
	//
	// The hook runs while the server room-map lock is held, so
	// implementations must return promptly; defer heavy I/O to a
	// goroutine if needed. The doc passed in is owned by the server —
	// retaining a reference past the hook return is safe as long as the
	// caller serialises access through Transact / public APIs.
	OnLoadDocument func(ctx context.Context, room string, doc *crdt.Doc) error

	// OnUnloadDocument, if non-nil, is called once per room immediately
	// after the room has been evicted from the server's in-memory map.
	// Fires from both handleDisconnect (last-peer-leaves) and CloseRoom.
	// Use this to release per-room caches, flush metrics, or notify
	// downstream systems that the doc is no longer hot. (#60)
	OnUnloadDocument func(ctx context.Context, room string)

	// OnFirstPeer, if non-nil, fires when a room transitions from 0 to 1
	// peers — i.e. the first peer just joined this active session of the
	// document. Useful for warm-up tasks (preloading caches, opening
	// downstream connections). Fires after the peer has been registered
	// with the room and after all server locks have been released. ctx is
	// the WebSocket request context; it is cancelled when the peer's HTTP
	// request is cancelled. (#60)
	//
	// Note: under heavy churn the (OnFirstPeer / OnLastPeer) pair for the
	// same room may interleave out of strict time order — implementations
	// must be idempotent against repeated transitions.
	OnFirstPeer func(ctx context.Context, room string)

	// OnLastPeer, if non-nil, fires when a room transitions from 1 to 0
	// peers — i.e. the last peer just disconnected. Useful for cool-down
	// tasks (releasing caches, closing downstream connections, scheduling
	// the eventual OnUnloadDocument). Fires before OnUnloadDocument when
	// both apply. ctx is context.Background() — the WS request that owned
	// the peer has already terminated by this point. (#60)
	OnLastPeer func(ctx context.Context, room string)

	// MaxUpdateBytes is the maximum size of a single V1 update that
	// BroadcastUpdate will fan out, or that Apply will produce and
	// fan out. Zero means use the same 64 MiB default applied to
	// WebSocket peer frames (maxWSMessageBytes).
	MaxUpdateBytes int

	// MaxRooms caps the total number of rooms the server will hold at
	// once, across both peer-upgrade-created and Apply-created rooms.
	// Zero means unlimited. Enforcement applies uniformly: peer upgrades
	// past the cap receive HTTP 503; Apply past the cap returns
	// ErrTooManyRooms.
	MaxRooms int

	// MaxMessageBytes is the per-message size cap on the WebSocket read path.
	// Frames larger than this are rejected by the underlying gorilla/websocket
	// library (which closes the connection with code 1009). Zero (the default)
	// uses the package default of 64 MiB, which matches Rust yrs-warp's underlying
	// warp default. Yjs JS's y-websocket inherits ws library's 100 MiB default.
	//
	// Lower this for stricter limits in untrusted multi-tenant deployments;
	// raise it for unusual bulk-sync workloads.
	MaxMessageBytes int64

	// MessageRateLimit caps the sustained inbound-message rate (messages per
	// second) for each peer. Zero (the default) means unlimited, preserving
	// existing behaviour. When set, every peer gets its own token-bucket limiter;
	// a peer that exceeds it is disconnected (issue #51). Disconnect — rather than
	// dropping the offending message — is deliberate: silently discarding a CRDT
	// update would leave that peer permanently diverged.
	MessageRateLimit rate.Limit

	// MessageRateBurst is the token-bucket burst size paired with
	// MessageRateLimit (how many messages may arrive back-to-back before the
	// sustained rate applies). Ignored when MessageRateLimit is zero. Zero or
	// negative with a non-zero MessageRateLimit defaults to defaultRateBurst.
	MessageRateBurst int

	// Logger receives structured log entries for connection lifecycle, write
	// failures, slow-peer disconnects, and persistence errors. nil falls back
	// to slog.Default(). Most operators want to wire this to their app logger
	// rather than rely on the default.
	Logger *slog.Logger

	// PeerWriteQueueSize is the buffer capacity of each peer's broadcast
	// write queue. When the queue fills (slow peer / dead connection), the
	// peer is disconnected — forcing them to reconnect and re-sync via the
	// CRDT's pending-structs machinery. Matches yrs-warp's bounded-broadcast
	// pattern.
	//
	// Zero (the default) uses 256, sized for typical sync workloads.
	PeerWriteQueueSize int

	// MaxPendingItems caps the per-document pending-items queue depth. The
	// queue holds items whose dependencies have not yet arrived, waiting for
	// out-of-order delivery to resolve. Zero or negative uses the crdt default
	// (100,000). See crdt.WithMaxPendingItems and issue #46.
	MaxPendingItems int

	// HandshakeTimeout caps how long a peer may stay connected without sending
	// any message after the WebSocket upgrade completes. This is the first-line
	// defense against slow-loris-style attacks where an attacker completes the
	// handshake on many connections and then sends nothing, holding goroutines
	// and buffers indefinitely. After the first successful ReadMessage the
	// deadline is cleared. Zero or negative uses the default (30 seconds).
	// See #47.
	HandshakeTimeout time.Duration

	// MaxAwarenessBytesPerRoom caps the cumulative byte size of awareness
	// state held in one room across all remote clients. Without this cap a
	// single peer can claim up to maxAwarenessClientsPerPeer (10,000)
	// clientIDs each holding the maximum per-state size (1 MiB) — up to
	// ~10 GiB of awareness state in one room. Incoming entries that would
	// push the total past this cap are silently dropped (matching the
	// existing oversized-state handling). Zero (the default) disables the
	// cap. Suggested production value: 100 MiB. See issue #48 vector B.
	MaxAwarenessBytesPerRoom int64

	// MaxAwarenessClientsPerRoom caps the number of DISTINCT awareness client
	// entries tracked in one room (live presence plus retained removal
	// tombstones). Without it a peer can invent unbounded client IDs — including
	// null-state entries, which bypass MaxAwarenessBytesPerRoom — to exhaust
	// memory. Previously-unseen client IDs past this cap are dropped. Zero (the
	// default) disables the cap. Suggested production value: 10,000.
	MaxAwarenessClientsPerRoom int

	// AwarenessExpiry, when > 0, starts a per-room background sweep that marks a
	// remote client's presence as removed if no update for it arrives within this
	// duration. It reclaims "ghost" presence from peers that died silently
	// (mobile sleep, NAT timeout, half-open TCP) without a clean disconnect.
	// Zero (the default) disables auto-expiry. The sweep goroutine is stopped
	// when the room is evicted.
	//
	// Set this comfortably ABOVE the clients' presence keep-alive interval, or a
	// still-connected client will be expired between its keep-alives. Yjs clients
	// re-announce local presence roughly every 15s (half the y-protocols 30s
	// outdated-timeout), and that re-announce — including for a peer attached to
	// another cluster node, since awareness is relayed — refreshes the entry's
	// last-update time here. The default suggested value 30s leaves ample margin;
	// values at or below ~15s risk flapping live peers offline.
	AwarenessExpiry time.Duration

	// connSem enforces MaxConnections as a hard cap. Lazily initialised on
	// first ServeHTTP. nil when MaxConnections == 0 (unlimited).
	connSem     *semaphore.Weighted
	connSemOnce sync.Once
}

// connSemaphore lazily initialises and returns the server-wide connection
// semaphore. Returns nil when MaxConnections == 0 (unlimited).
func (s *Server) connSemaphore() *semaphore.Weighted {
	s.connSemOnce.Do(func() {
		if s.MaxConnections > 0 {
			s.connSem = semaphore.NewWeighted(int64(s.MaxConnections))
		}
	})
	return s.connSem
}

// checkOrigin validates the WebSocket upgrade request's Origin header.
// When AllowedOrigins is empty, a same-origin check is performed (Origin host
// must equal the HTTP Host header). Non-browser clients that omit Origin are
// always allowed. Use AllowedOrigins = []string{"*"} to allow any origin.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients (curl, native apps) don't send Origin; permit them.
		return true
	}
	if len(s.AllowedOrigins) == 0 {
		// Same-origin fallback: compare the origin's host to the HTTP Host header.
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	for _, allowed := range s.AllowedOrigins {
		if originMatches(allowed, origin) {
			return true
		}
	}
	return false
}

// originMatches reports whether origin matches an AllowedOrigins entry,
// case-insensitively. An entry without "*" must equal origin exactly. An entry
// may contain one or more "*" wildcards, each matching any (possibly empty) run
// of characters, so "https://*.example.com" matches "https://app.example.com"
// and "https://pr-*---web-*.run.app" matches "https://pr-12---web-abc.run.app".
// A bare "*" matches any origin. The first and last literal segments are
// anchored to the start and end of origin, so a wildcard cannot be used to spoof
// a different host (e.g. "https://*.example.com" does not match
// "https://x.example.com.evil"). A trailing "*" is restricted to an optional
// ":<port>": "https://app.example.com*" matches "https://app.example.com" and
// "https://app.example.com:8443" but not "https://app.example.com.evil".
func originMatches(pattern, origin string) bool {
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, origin)
	}
	p := strings.ToLower(pattern)
	o := strings.ToLower(origin)
	// A trailing "*" (other than the bare "*" allow-all) must NOT act as an
	// unanchored suffix — otherwise "https://app.example.com*" would also match
	// "https://app.example.com.evil", a CORS allow-list bypass (#129 review).
	// Restrict it to an optional ":<port>" so it only covers the "host with
	// optional port" case it is intended for.
	trailingStar := p != "*" && strings.HasSuffix(p, "*")
	segs := strings.Split(p, "*")
	// First literal segment must be a prefix.
	if !strings.HasPrefix(o, segs[0]) {
		return false
	}
	o = o[len(segs[0]):]
	// Last literal segment must be a suffix.
	last := segs[len(segs)-1]
	if !strings.HasSuffix(o, last) {
		return false
	}
	o = o[:len(o)-len(last)]
	// Middle literal segments must occur in order.
	for _, mid := range segs[1 : len(segs)-1] {
		i := strings.Index(o, mid)
		if i < 0 {
			return false
		}
		o = o[i+len(mid):]
	}
	if trailingStar {
		// Whatever remains is what the trailing "*" matched; allow only an
		// optional ":<port>" so it cannot extend onto a different host.
		return isOptionalPort(o)
	}
	return true
}

// isOptionalPort reports whether s is empty or a ":<digits>" port suffix. It
// bounds what a trailing "*" in an AllowedOrigins pattern may match so the
// wildcard cannot be abused to match a different host.
func isOptionalPort(s string) bool {
	if s == "" {
		return true
	}
	if len(s) < 2 || s[0] != ':' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isValidRoomName reports whether name is a safe, non-empty room identifier.
// The rule is centralised in internal/roomname so the HTTP provider enforces
// identical limits (issue #50).
func isValidRoomName(name string) bool {
	return roomname.Valid(name)
}

// defaultRateBurst is the token-bucket burst used when MessageRateLimit is set
// but MessageRateBurst is not — enough slack to absorb a client's initial
// sync handshake batch without tripping the sustained-rate limit.
const defaultRateBurst = 32

// newPeerLimiter returns a per-peer inbound-message limiter, or nil when rate
// limiting is disabled (MessageRateLimit == 0).
func (s *Server) newPeerLimiter() *rate.Limiter {
	if s.MessageRateLimit <= 0 {
		return nil
	}
	burst := s.MessageRateBurst
	if burst <= 0 {
		burst = defaultRateBurst
	}
	return rate.NewLimiter(s.MessageRateLimit, burst)
}

// NewServer returns a new Server with an empty room store and no persistence.
func NewServer() *Server {
	s := &Server{
		rooms:      make(map[string]*room),
		shutdownCh: make(chan struct{}),
	}
	s.upgrader = gws.Upgrader{CheckOrigin: s.checkOrigin}
	return s
}

// Shutdown closes all active peer connections and waits for their goroutines
// to exit or for ctx to expire. Call this during server shutdown to prevent
// goroutine leaks and ensure in-flight operations complete cleanly.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })

	// Stop THIS server's relay delivery by cancelling the relay context: this
	// winds down the relay worker and the relay's per-node delivery goroutine
	// (started under relayCtx). It does NOT Close the relay — the caller owns
	// the relay lifetime and must Close() it once every attached server is done,
	// because a single relay is commonly shared across multiple in-process
	// Servers (the MemRelay pattern) and Closing it would stop delivery for all
	// of them (FIX C). No-op when no relay is attached.
	if s.relayCancel != nil {
		s.relayCancel()
	}

	// Collect all active peer connections and persistence channels.
	s.rmu.RLock()
	var conns []*gws.Conn
	var persistDones []chan struct{}
	for _, r := range s.rooms {
		r.mu.Lock()
		for p := range r.peers {
			conns = append(conns, p.conn)
		}
		r.mu.Unlock()
		if r.persistDone != nil {
			persistDones = append(persistDones, r.persistDone)
		}
	}
	s.rmu.RUnlock()

	// Close each connection. The peer read loop will exit on the next
	// ReadMessage call, triggering handleDisconnect cleanup.
	for _, c := range conns {
		if err := c.Close(); err != nil {
			s.log().Debug("shutdown close failed", "err", err)
		}
	}

	// Wait for all persistence goroutines to drain in-flight writes.
	// Disconnect handlers (triggered by the connection closes above) signal
	// persistence goroutines to stop as rooms become empty.
	done := make(chan struct{})
	go func() {
		for _, ch := range persistDones {
			<-ch
		}
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}

	return ctx.Err()
}

// NewServerWithPersistence returns a Server that loads and stores room state
// via the given PersistenceAdapter on every room creation and transaction.
func NewServerWithPersistence(p PersistenceAdapter) *Server {
	s := NewServer()
	s.persistence = p
	return s
}

// GetDoc returns the document for the given room, or nil if no peer has
// connected to that room yet.
func (s *Server) GetDoc(name string) *crdt.Doc {
	s.rmu.RLock()
	defer s.rmu.RUnlock()
	if r, ok := s.rooms[name]; ok {
		return r.doc
	}
	return nil
}

func (s *Server) getOrCreateRoom(ctx context.Context, name string) (*room, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if r, ok := s.rooms[name]; ok {
		return r, nil
	}
	if s.MaxRooms > 0 && len(s.rooms) >= s.MaxRooms {
		return nil, ErrTooManyRooms
	}
	docOpts := []crdt.DocOption{}
	if s.MaxPendingItems > 0 {
		docOpts = append(docOpts, crdt.WithMaxPendingItems(s.MaxPendingItems))
	}
	aw := awareness.New(0)
	if s.MaxAwarenessBytesPerRoom > 0 {
		aw.SetMaxBytes(s.MaxAwarenessBytesPerRoom)
	}
	if s.MaxAwarenessClientsPerRoom > 0 {
		aw.SetMaxClients(s.MaxAwarenessClientsPerRoom)
	}
	if s.AwarenessExpiry > 0 {
		// Background sweep; stopped via aw.Destroy() when the room is evicted.
		aw.StartAutoExpiry(s.AwarenessExpiry)
	}
	r := &room{
		doc:       crdt.New(docOpts...),
		awareness: aw,
		peers:     make(map[*peer]struct{}),
	}
	if s.MaxPeersPerRoom > 0 {
		r.peerSem = semaphore.NewWeighted(int64(s.MaxPeersPerRoom))
	}
	if s.persistence != nil {
		data, err := s.persistence.LoadDoc(name)
		if err != nil {
			return nil, fmt.Errorf("loading room %q: %w", name, err)
		}
		if len(data) > 0 {
			if err := crdt.ApplyUpdateV1(r.doc, data, nil); err != nil {
				return nil, fmt.Errorf("bootstrapping room %q: %w", name, err)
			}
		}
	}
	// #60 — fire OnLoadDocument AFTER persistence bootstrap but BEFORE the
	// persistence worker starts and the room is registered, so a hook
	// returning an error fails room creation cleanly with nothing left to
	// clean up. Hook runs under s.rmu.Lock; implementations must be fast.
	// A panic in the hook is recovered (logged at Error with the stack)
	// and treated as a hook-failure error so room creation is not silently
	// committed in an inconsistent state.
	if hook := s.OnLoadDocument; hook != nil {
		var hookErr error
		s.safeHook("OnLoadDocument", func() {
			hookErr = hook(ctx, name, r.doc)
		})
		if hookErr != nil {
			return nil, fmt.Errorf("OnLoadDocument for room %q: %w", name, hookErr)
		}
	}
	if s.persistence != nil {
		// Serialise persistence writes through a buffered channel so that a
		// slow storage backend does not block the Transact caller (N-H7) and
		// writes arrive in order.
		r.persistCh = make(chan []byte, 256)
		r.persistStop = make(chan struct{})
		r.persistDone = make(chan struct{})
		s.startPersistenceWorker(r, name)
		r.doc.OnUpdate(func(update []byte, _ any) {
			select {
			case r.persistCh <- update:
			case <-r.persistStop:
			}
		})
	}
	// Wire relay observers (doc.OnUpdate + awareness.OnChange) so local changes
	// are published to other nodes. Registered under s.rmu.Lock (held by the
	// caller) before the room is published into s.rooms, so no change is missed.
	// No-op when no relay is attached.
	if s.relay != nil {
		s.registerRelayObservers(r, name)
		s.relay.RoomActivated(name)
	}
	s.rooms[name] = r
	return r, nil
}

// ServeHTTP upgrades the request to WebSocket and runs the peer sync loop.
// Room name is taken from the {room} path variable (Go 1.22 ServeMux) or
// falls back to the last path segment.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.AuthFunc != nil && !s.AuthFunc(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	name := r.PathValue("room")
	if name == "" {
		name = path.Base(r.URL.Path)
	}
	if !isValidRoomName(name) {
		http.Error(w, "invalid room name", http.StatusBadRequest)
		return
	}

	rm, err := s.getOrCreateRoom(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrTooManyRooms) {
			http.Error(w, "too many rooms", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "room unavailable", http.StatusInternalServerError)
		return
	}

	// Enforce per-room and server-wide connection limits before upgrading so
	// that rejected requests get a clean HTTP 503 rather than an abrupt close
	// after the WebSocket handshake (N-H5).
	// semaphore.Weighted.TryAcquire provides a hard guarantee: never more than
	// the configured cap simultaneously, regardless of burst pattern.
	if rm.peerSem != nil && !rm.peerSem.TryAcquire(1) {
		s.log().Debug("MaxPeersPerRoom cap reached", "room", name)
		http.Error(w, "room full", http.StatusServiceUnavailable)
		return
	}
	if sem := s.connSemaphore(); sem != nil && !sem.TryAcquire(1) {
		if rm.peerSem != nil {
			rm.peerSem.Release(1) // release per-room ticket we just acquired
		}
		s.log().Debug("MaxConnections cap reached")
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		if rm.peerSem != nil {
			rm.peerSem.Release(1)
		}
		if sem := s.connSemaphore(); sem != nil {
			sem.Release(1)
		}
		return
	}
	// Reject frames larger than maxWSMessageBytes before buffering them.
	// Without this, a single 4 GB frame would be fully read into memory before
	// any application-level validation could reject it.
	ws.SetReadLimit(s.maxMessageBytes())

	p := &peer{
		conn:       ws,
		room:       rm,
		roomName:   name,
		server:     s,
		done:       make(chan struct{}),
		clientIDs:  make(map[uint64]struct{}),
		writeCh:    make(chan []byte, s.peerWriteQueueSize()),
		writerDone: make(chan struct{}),
		limiter:    s.newPeerLimiter(),
	}

	// Verify the room is still in the server map before adding the peer.
	// Holding rmu.RLock prevents handleDisconnect from deleting the room
	// (it needs rmu.Lock), closing the TOCTOU window between getOrCreateRoom
	// and peer addition.
	s.rmu.RLock()
	if current, ok := s.rooms[name]; !ok || current != rm {
		s.rmu.RUnlock()
		if rm.peerSem != nil {
			rm.peerSem.Release(1)
		}
		if sem := s.connSemaphore(); sem != nil {
			sem.Release(1)
		}
		_ = ws.Close() // close errors during teardown are expected; not logged
		return
	}
	rm.mu.Lock()
	rm.peers[p] = struct{}{}
	firstPeer := len(rm.peers) == 1 // #60: 0→1 transition
	rm.mu.Unlock()
	s.rmu.RUnlock()

	// #60 — OnFirstPeer fires after all server locks are released so the
	// hook is free to do blocking work (warm caches, open connections,
	// emit metrics) without holding up other peers from joining. A panic
	// in the hook is recovered + logged; the peer handshake below
	// continues regardless.
	if firstPeer {
		if hook := s.OnFirstPeer; hook != nil {
			s.safeHook("OnFirstPeer", func() { hook(r.Context(), name) })
		}
	}

	// Start the per-peer writer ONLY after the peer is registered with the
	// room. From this point handleDisconnect (registered next) owns the
	// runWriter teardown via close(writeCh) + <-writerDone. Before this
	// point, a TOCTOU loss returned without cleanup, leaking runWriter (#33).
	go p.runWriter()

	defer func() {
		close(p.done) // H1: unblock the context-watcher goroutine
		p.handleDisconnect()
		_ = ws.Close() // close errors during teardown are expected; not logged
	}()

	// Close the WebSocket when the HTTP request context is cancelled
	// (e.g. graceful server shutdown via Shutdown, or client disconnect
	// detected by the HTTP layer). This unblocks the read loop below.
	ctx := r.Context()
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close() // close errors during teardown are expected; not logged
		case <-s.shutdownCh:
			_ = ws.Close() // close errors during teardown are expected; not logged
		case <-p.done: // H1: read loop exited normally; nothing to do
		}
	}()

	// 1. Send sync step-1 — request the peer's state vector.
	p.sendSync(ygsync.EncodeSyncStep1(rm.doc))

	// 2. Send sync step-2 — give the peer everything the server already has.
	fullUpdate := crdt.EncodeStateAsUpdateV1(rm.doc, nil)
	step2 := encodeSyncStep2Msg(fullUpdate)
	p.sendSync(step2)

	// 3. Send the current awareness state of all active peers.
	p.sendAwareness(rm.awareness.EncodeUpdate(nil))

	// Read loop — exits when the connection is closed (by peer, by context
	// cancellation, or by Shutdown).
	//
	// An initial read deadline guards against slow-loris: a peer that completes
	// the WebSocket handshake but never sends a message would otherwise hold
	// the read goroutine, writeCh buffer, and any connection-tracking memory
	// indefinitely. After the first successful ReadMessage we clear the
	// deadline; downstream slow-peer protection is handled by the writeCh
	// disconnect-on-overflow path (see #19) and gorilla/websocket's pong
	// handling. See #47.
	if err := ws.SetReadDeadline(time.Now().Add(s.handshakeTimeout())); err != nil {
		return
	}
	firstMessage := true
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if firstMessage {
			// Clear the handshake deadline; subsequent reads can take as long
			// as the WebSocket protocol's own pong-timeout machinery allows.
			if err := ws.SetReadDeadline(time.Time{}); err != nil {
				return
			}
			firstMessage = false
		}
		// Per-peer inbound rate limit (#51). On exceed, disconnect rather than
		// drop the message: silently discarding a CRDT update would leave this
		// peer permanently diverged.
		if p.limiter != nil && !p.limiter.Allow() {
			p.server.log().Warn("disconnecting peer: inbound message rate limit exceeded",
				"room", p.roomName)
			break
		}
		p.handleMessage(data)
	}
}

// encodeSyncStep2Msg builds a sync step-2 wire message from a raw update blob.
func encodeSyncStep2Msg(update []byte) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgSyncStep2)
	enc.WriteVarBytes(update)
	return enc.Bytes()
}
