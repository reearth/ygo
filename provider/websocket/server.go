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
	"net/http"
	"path"
	"sync"

	gws "github.com/gorilla/websocket"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// Outer message type codes defined by y-protocols / y-websocket.
const (
	msgSync           = uint64(0)
	msgAwareness      = uint64(1)
	msgQueryAwareness = uint64(3)
)

// room holds the shared document and awareness state for one named room.
type room struct {
	mu        sync.Mutex
	doc       *crdt.Doc
	awareness *awareness.Awareness
	peers     map[*peer]struct{}
}

// peer is one connected WebSocket client.
type peer struct {
	conn      *gws.Conn
	wmu       sync.Mutex // serialises concurrent writes
	room      *room
	clientIDs map[uint64]struct{} // awareness clientIDs controlled by this peer
	cidMu     sync.Mutex
}

// Server is a net/http-compatible WebSocket handler.
// Each distinct room name maps to an independent Yjs document.
type Server struct {
	upgrader gws.Upgrader
	rmu      sync.RWMutex
	rooms    map[string]*room
}

// NewServer returns a new Server with an empty room store.
func NewServer() *Server {
	return &Server{
		upgrader: gws.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		rooms: make(map[string]*room),
	}
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

func (s *Server) getOrCreateRoom(name string) *room {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if r, ok := s.rooms[name]; ok {
		return r
	}
	r := &room{
		doc:       crdt.New(),
		awareness: awareness.New(0),
		peers:     make(map[*peer]struct{}),
	}
	s.rooms[name] = r
	return r
}

// ServeHTTP upgrades the request to WebSocket and runs the peer sync loop.
// Room name is taken from the {room} path variable (Go 1.22 ServeMux) or
// falls back to the last path segment.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("room")
	if name == "" {
		name = path.Base(r.URL.Path)
	}

	rm := s.getOrCreateRoom(name)

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	p := &peer{
		conn:      ws,
		room:      rm,
		clientIDs: make(map[uint64]struct{}),
	}

	rm.mu.Lock()
	rm.peers[p] = struct{}{}
	rm.mu.Unlock()

	defer func() {
		p.handleDisconnect()
		ws.Close()
	}()

	// 1. Send sync step-1 — request the peer's state vector.
	p.sendSync(ygsync.EncodeSyncStep1(rm.doc))

	// 2. Send sync step-2 — give the peer everything the server already has.
	fullUpdate := crdt.EncodeStateAsUpdateV1(rm.doc, nil)
	step2 := encodeSyncStep2Msg(fullUpdate)
	p.sendSync(step2)

	// 3. Send the current awareness state of all active peers.
	p.sendAwareness(rm.awareness.EncodeUpdate(nil))

	// Read loop.
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
		_ = p.room.awareness.ApplyUpdate(awBytes, p)
		p.broadcastAwareness(awBytes)

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
		p.clientIDs[clientID] = struct{}{}
	}
}

// handleDisconnect removes the peer from the room and broadcasts awareness
// removal for all clientIDs the peer owned.
func (p *peer) handleDisconnect() {
	p.room.mu.Lock()
	delete(p.room.peers, p)
	p.room.mu.Unlock()

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

	for _, other := range targets {
		other.write(data)
	}
}

// write sends a raw binary WebSocket message, serialising concurrent writes.
func (p *peer) write(data []byte) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.conn.WriteMessage(gws.BinaryMessage, data)
}
