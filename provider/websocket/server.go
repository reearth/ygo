package websocket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"
	"golang.org/x/sync/semaphore"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// writeTimeout is applied to every individual WebSocket write. A peer that
// stops reading will be detected and disconnected within this window, preventing
// a slow-reader from blocking the broadcast loop for all other peers.
const writeTimeout = 10 * time.Second

// Outer message type codes defined by y-protocols / y-websocket.
const (
	msgSync           = uint64(0)
	msgAwareness      = uint64(1)
	msgAuth           = uint64(2) // y-websocket auth; silently ignored
	msgQueryAwareness = uint64(3)
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
}

// Server is a net/http-compatible WebSocket handler.
// Each distinct room name maps to an independent Yjs document.
type Server struct {
	upgrader    gws.Upgrader
	rmu         sync.RWMutex
	rooms       map[string]*room
	persistence PersistenceAdapter

	shutdownOnce sync.Once
	shutdownCh   chan struct{} // closed by Shutdown

	// AuthFunc, if non-nil, is called before upgrading each incoming WebSocket
	// connection. Return false to reject the connection; the server responds
	// with 401 Unauthorized. Use this hook for token validation, session checks,
	// or IP allow-lists. If nil, all connections are accepted.
	AuthFunc func(r *http.Request) bool

	// AllowedOrigins is the list of origins permitted to open WebSocket
	// connections (C2 — CORS). Each entry must be a full origin string, e.g.
	// "https://example.com". Use "*" to allow any origin.
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
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

// isValidRoomName reports whether name is a safe, non-empty room identifier.
// Rejected: empty string, names exceeding 255 bytes, names consisting solely
// of "." or ".." (path traversal), and names containing control characters
// (runes < 0x20). All other printable content, including spaces and Unicode,
// is permitted — matching the permissive behavior of the y-websocket JS server.
func isValidRoomName(name string) bool {
	if len(name) == 0 || len(name) > 255 {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r < 0x20 {
			return false
		}
	}
	return true
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

func (s *Server) getOrCreateRoom(name string) (*room, error) {
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

	rm, err := s.getOrCreateRoom(name)
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
	rm.mu.Unlock()
	s.rmu.RUnlock()

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
