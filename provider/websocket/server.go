// Package websocket provides a net/http-compatible WebSocket handler that
// synchronises Yjs documents between multiple peers using the y-protocols
// sync and awareness protocols.
//
// Usage:
//
//	srv := websocket.NewServer()
//	http.Handle("/yjs/{room}", srv)
//	http.ListenAndServe(":8080", nil)
package websocket

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gws "github.com/gorilla/websocket"

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

// InjectOrigin is the transaction origin used by InjectUpdate. The persistence
// callback checks for this origin and skips DB writes for injected updates,
// since the caller is responsible for persistence. This prevents deadlocks
// when the caller holds a database write lock (e.g., SQLite single-writer).
var InjectOrigin any = "ygo:inject"

// maxAwarenessClientsPerPeer caps the number of awareness clientIDs one peer
// may claim ownership of. Without this cap an attacker can send an awareness
// update listing 1,000,000 clientIDs and cause an OOM when handleDisconnect
// builds the removal slice (N-H4).
const maxAwarenessClientsPerPeer = 10_000

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
	mu          sync.Mutex
	doc         *crdt.Doc
	awareness   *awareness.Awareness
	peers       map[*peer]struct{}
	activePeers atomic.Int64 // atomic; live peers in this room

	// Persistence write queue. nil when no PersistenceAdapter is configured.
	persistCh   chan []byte   // buffered channel for serialised writes
	persistStop chan struct{} // closed to signal goroutine to drain and exit
	persistDone chan struct{} // closed when persistence goroutine exits

	// Server-side update injection queue. Updates queued here are applied
	// to the document and broadcast to all peers by a dedicated goroutine,
	// avoiding lock contention with the WebSocket read loops.
	injectCh   chan []byte   // buffered channel for server-side updates
	injectStop chan struct{} // closed to signal goroutine to drain and exit
	injectDone chan struct{} // closed when inject goroutine exits
}

// peer is one connected WebSocket client.
type peer struct {
	conn      *gws.Conn
	wmu       sync.Mutex // serialises concurrent writes
	closed    bool       // H2: true after handleDisconnect; guarded by wmu
	room      *room
	roomName  string              // C1: name used to delete room when empty
	server    *Server             // C1: back-reference for room map cleanup
	done      chan struct{}       // H1: closed when the read loop exits
	clientIDs map[uint64]struct{} // awareness clientIDs controlled by this peer
	cidMu     sync.Mutex
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
	AllowedOrigins []string

	// MaxConnections is the server-wide cap on simultaneous WebSocket peers.
	// Upgrade requests that would exceed this limit are rejected with 503.
	// Zero (the default) means unlimited (N-H5).
	MaxConnections int

	// MaxPeersPerRoom is the per-room cap on simultaneous WebSocket peers.
	// Upgrade requests that would exceed this limit are rejected with 503.
	// Zero (the default) means unlimited (N-H5).
	MaxPeersPerRoom int

	activeConns atomic.Int64 // atomic; total live WebSocket connections
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
		if r.injectDone != nil {
			persistDones = append(persistDones, r.injectDone)
		}
	}
	s.rmu.RUnlock()

	// Close each connection. The peer read loop will exit on the next
	// ReadMessage call, triggering handleDisconnect cleanup.
	for _, c := range conns {
		_ = c.Close()
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

// InjectUpdate queues a binary V1 update to be applied to the document in
// the named room and broadcast to all connected WebSocket peers. If no room
// exists yet, one is created (and the update becomes the initial state).
//
// The update is processed asynchronously by a dedicated goroutine that
// serialises injected updates with peer-originated messages, avoiding lock
// contention with the WebSocket read loop.
//
// This is the safe way to inject server-side changes into a live document
// without connecting as a WebSocket peer.
//
// The update must be a valid Yjs V1 encoded update (e.g. from
// Doc.EncodeStateAsUpdate on another Doc).
func (s *Server) InjectUpdate(name string, update []byte) error {
	rm, err := s.getOrCreateRoom(name)
	if err != nil {
		return err
	}

	if rm.injectCh == nil {
		// Room was created before InjectUpdate support — shouldn't happen
		// with current code, but handle gracefully.
		return fmt.Errorf("room %q has no injection channel", name)
	}

	// Queue the update — non-blocking (buffered channel).
	select {
	case rm.injectCh <- update:
		return nil
	default:
		return fmt.Errorf("injection channel full for room %q", name)
	}
}

func (s *Server) getOrCreateRoom(name string) (*room, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if r, ok := s.rooms[name]; ok {
		return r, nil
	}
	r := &room{
		doc:       crdt.New(),
		awareness: awareness.New(0),
		peers:     make(map[*peer]struct{}),
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
		go func() {
			defer close(r.persistDone)
			store := func(update []byte) {
				defer func() {
					if rv := recover(); rv != nil {
						log.Printf("ygo/websocket: StoreUpdate panic for room %q: %v", name, rv)
					}
				}()
				if err := s.persistence.StoreUpdate(name, update); err != nil {
					log.Printf("ygo/websocket: StoreUpdate for room %q: %v", name, err)
				}
			}
			for {
				select {
				case update := <-r.persistCh:
					store(update)
				case <-r.persistStop:
					// Drain buffered updates before exiting.
					for {
						select {
						case update := <-r.persistCh:
							store(update)
						default:
							return
						}
					}
				}
			}
		}()
		r.doc.OnUpdate(func(update []byte, origin any) {
			// Skip persistence for server-injected updates — the caller
			// already wrote to the DB. Without this filter, the persistence
			// goroutine would try to acquire a SQLite write lock while the
			// caller's HTTP handler also holds one, causing a deadlock.
			if origin == InjectOrigin {
				return
			}
			select {
			case r.persistCh <- update:
			case <-r.persistStop:
			}
		})
	}
	// Start the injection goroutine for server-side updates.
	// This goroutine applies updates and broadcasts to peers without
	// competing with WebSocket read loops for the doc lock.
	r.injectCh = make(chan []byte, 64)
	r.injectStop = make(chan struct{})
	r.injectDone = make(chan struct{})
	go func() {
		defer close(r.injectDone)
		for {
			select {
			case update := <-r.injectCh:
				// Apply the update to the doc. This acquires d.mu.Lock
				// but runs in its own goroutine so it waits for the
				// read loop to release the lock between messages.
				if err := crdt.ApplyUpdateV1(r.doc, update, InjectOrigin); err != nil {
					log.Printf("ygo/websocket: InjectUpdate for room %q: %v", name, err)
					continue
				}

				// Broadcast to all connected peers.
				syncMsg := encodeSyncStep2Msg(update)
				r.mu.Lock()
				targets := make([]*peer, 0, len(r.peers))
				for p := range r.peers {
					targets = append(targets, p)
				}
				r.mu.Unlock()
				for _, p := range targets {
					go p.write(syncMsg)
				}

			case <-r.injectStop:
				// Drain remaining updates before exiting.
				for {
					select {
					case update := <-r.injectCh:
						_ = crdt.ApplyUpdateV1(r.doc, update, InjectOrigin)
					default:
						return
					}
				}
			}
		}
	}()

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
		http.Error(w, "room unavailable", http.StatusInternalServerError)
		return
	}

	// Enforce per-room and server-wide connection limits before upgrading so
	// that rejected requests get a clean HTTP 503 rather than an abrupt close
	// after the WebSocket handshake (N-H5).
	// Atomics are incremented optimistically before the upgrade; on failure
	// they are decremented so the counts stay accurate (fixes TOCTOU race).
	if s.MaxPeersPerRoom > 0 {
		if rm.activePeers.Add(1) > int64(s.MaxPeersPerRoom) {
			rm.activePeers.Add(-1)
			http.Error(w, "room full", http.StatusServiceUnavailable)
			return
		}
	}
	if s.MaxConnections > 0 {
		if s.activeConns.Add(1) > int64(s.MaxConnections) {
			s.activeConns.Add(-1)
			if s.MaxPeersPerRoom > 0 {
				rm.activePeers.Add(-1) // undo per-room increment
			}
			http.Error(w, "too many connections", http.StatusServiceUnavailable)
			return
		}
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		if s.MaxPeersPerRoom > 0 {
			rm.activePeers.Add(-1)
		}
		if s.MaxConnections > 0 {
			s.activeConns.Add(-1)
		}
		return
	}
	// Reject frames larger than maxWSMessageBytes before buffering them.
	// Without this, a single 4 GB frame would be fully read into memory before
	// any application-level validation could reject it.
	ws.SetReadLimit(maxWSMessageBytes)

	p := &peer{
		conn:      ws,
		room:      rm,
		roomName:  name,
		server:    s,
		done:      make(chan struct{}),
		clientIDs: make(map[uint64]struct{}),
	}

	// Verify the room is still in the server map before adding the peer.
	// Holding rmu.RLock prevents handleDisconnect from deleting the room
	// (it needs rmu.Lock), closing the TOCTOU window between getOrCreateRoom
	// and peer addition.
	s.rmu.RLock()
	if current, ok := s.rooms[name]; !ok || current != rm {
		s.rmu.RUnlock()
		if s.MaxPeersPerRoom > 0 {
			rm.activePeers.Add(-1)
		}
		if s.MaxConnections > 0 {
			s.activeConns.Add(-1)
		}
		_ = ws.Close()
		return
	}
	rm.mu.Lock()
	rm.peers[p] = struct{}{}
	rm.mu.Unlock()
	s.rmu.RUnlock()

	defer func() {
		close(p.done) // H1: unblock the context-watcher goroutine
		p.handleDisconnect()
		_ = ws.Close()
	}()

	// Close the WebSocket when the HTTP request context is cancelled
	// (e.g. graceful server shutdown via Shutdown, or client disconnect
	// detected by the HTTP layer). This unblocks the read loop below.
	ctx := r.Context()
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-s.shutdownCh:
			_ = ws.Close()
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
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
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

// handleMessage decodes the outer message type and dispatches accordingly.
func (p *peer) handleMessage(data []byte) {
	dec := encoding.NewDecoder(data)
	outerType, err := dec.ReadVarUint()
	if err != nil {
		return
	}

	switch outerType {
	case msgSync:
		// Sync payload follows directly (no VarBytes wrapper).
		payload := dec.RemainingBytes()
		reply, err := ygsync.ApplySyncMessage(p.room.doc, payload, p)
		if err != nil {
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
		// Awareness payload is VarBytes-wrapped (y-websocket protocol).
		awBytes, err := dec.ReadVarBytes()
		if err != nil {
			return
		}
		p.trackAwarenessClients(awBytes)
		if err := p.room.awareness.ApplyUpdate(awBytes, p); err != nil {
			return // Drop invalid awareness updates; do not broadcast.
		}
		p.broadcastAwareness(awBytes)

	case msgAuth:
		// Auth messages (type 2) are defined by y-websocket but not used by
		// this server. Silently ignore.

	case msgQueryAwareness:
		p.sendAwareness(p.room.awareness.EncodeUpdate(nil))
	}
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
func (p *peer) handleDisconnect() {
	// H2: mark closed so concurrent broadcast writes skip this peer.
	p.wmu.Lock()
	p.closed = true
	p.wmu.Unlock()

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
	if empty {
		delete(p.server.rooms, p.roomName)
		if rm.persistStop != nil {
			close(rm.persistStop)
		}
		if rm.injectStop != nil {
			close(rm.injectStop)
		}
	}
	rm.mu.Unlock()
	p.server.rmu.Unlock()

	// Decrement atomic connection counters now that the peer has left.
	if p.server.MaxPeersPerRoom > 0 {
		rm.activePeers.Add(-1)
	}
	if p.server.MaxConnections > 0 {
		p.server.activeConns.Add(-1)
	}

	// Wait for the persistence goroutine to drain buffered writes before the
	// room reference becomes garbage. This runs outside the locks above.
	if empty && rm.persistDone != nil {
		<-rm.persistDone
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
	_ = p.room.awareness.ApplyUpdate(removalBytes, nil)
	p.broadcastAwarenessFromRoom(removalBytes)
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
	enc := encoding.NewEncoder()
	enc.WriteVarUint(uint64(len(toRemove)))
	for _, item := range toRemove {
		enc.WriteVarUint(item.id)
		enc.WriteVarUint(item.clock + 1)
		enc.WriteVarString("null")
	}
	return enc.Bytes()
}

// sendSync writes a sync message (outer type 0, raw payload) to this peer.
func (p *peer) sendSync(syncMsg []byte) {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(msgSync)
	enc.WriteRaw(syncMsg) // sync payload is NOT VarBytes-wrapped
	p.write(enc.Bytes())
}

// sendAwareness writes an awareness message (outer type 1, VarBytes payload)
// to this peer.
func (p *peer) sendAwareness(awMsg []byte) {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(msgAwareness)
	enc.WriteVarBytes(awMsg) // awareness payload IS VarBytes-wrapped
	p.write(enc.Bytes())
}

// broadcastSync sends a sync message to all OTHER peers in the room.
func (p *peer) broadcastSync(syncMsg []byte) {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(msgSync)
	enc.WriteRaw(syncMsg)
	p.broadcast(enc.Bytes(), true)
}

// broadcastAwareness sends an awareness message to all OTHER peers in the room.
func (p *peer) broadcastAwareness(awMsg []byte) {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(msgAwareness)
	enc.WriteVarBytes(awMsg)
	p.broadcast(enc.Bytes(), true)
}

// broadcastAwarenessFromRoom sends an awareness message to ALL peers (called
// from disconnect handler which has already removed itself from the room).
func (p *peer) broadcastAwarenessFromRoom(awMsg []byte) {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(msgAwareness)
	enc.WriteVarBytes(awMsg)
	p.broadcast(enc.Bytes(), false)
}

// broadcast sends data to peers in the room. If excludeSelf is true, the
// calling peer is excluded (used for normal broadcasts). If false, all peers
// receive it (used for disconnect announcements).
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

	// Write to each peer concurrently so that a single slow or unresponsive
	// peer cannot stall the broadcast loop for all others (N-H6).
	// Each peer.write() holds peer.wmu and sets a per-write deadline, so
	// concurrent goroutines targeting different peers are safe. The data slice
	// is read-only after this point, so sharing it across goroutines is safe.
	for _, other := range targets {
		go other.write(data)
	}
}

// write sends a raw binary WebSocket message, serialising concurrent writes.
// A per-write deadline of writeTimeout is applied so that a slow or unresponsive
// peer does not block the broadcast loop for all other peers in the room.
// H2: skips the write if the peer has already been marked closed by handleDisconnect.
func (p *peer) write(data []byte) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.closed {
		return
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = p.conn.WriteMessage(gws.BinaryMessage, data)
}
