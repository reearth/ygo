package websocket

import (
	"github.com/reearth/ygo/awareness"
)

// GetAwareness returns the *awareness.Awareness for the given room, or
// (nil, false) if no room with that name is currently resident in the server.
//
// A room becomes resident once the first peer connects or an Apply call
// auto-creates it, and is evicted when the last peer disconnects (or
// CloseRoom is called). The returned pointer is owned by the server; callers
// must serialise access through the *awareness.Awareness public methods, all
// of which are internally synchronised.
//
// This accessor exists so external coordinators (e.g. a cluster relay) can
// merge inbound awareness updates into the canonical per-room state. It mirrors
// GetDoc, which exposes the room's *crdt.Doc.
func (s *Server) GetAwareness(room string) (*awareness.Awareness, bool) {
	s.rmu.RLock()
	r, ok := s.rooms[room]
	s.rmu.RUnlock()
	if !ok {
		return nil, false
	}
	// A room still performing its off-lock load (#182), or one whose load failed
	// (placeholder already removed), is reported as not-yet-resident — mirroring
	// GetDoc's contract — so callers never see the awareness of a room that is not
	// fully live.
	select {
	case <-r.ready:
		if r.loadErr != nil {
			return nil, false
		}
		return r.awareness, true
	default:
		return nil, false
	}
}

// Rooms returns the names of all rooms currently resident in the server, in
// unspecified order. The returned slice is a fresh copy; the caller owns it.
//
// The result is an immediately-stale snapshot — a room may be created or
// evicted concurrently. Intended for observability and for a cluster relay to
// enumerate the rooms it should subscribe to; do not use as a synchronization
// primitive.
func (s *Server) Rooms() []string {
	s.rmu.RLock()
	defer s.rmu.RUnlock()
	out := make([]string, 0, len(s.rooms))
	for name := range s.rooms {
		out = append(out, name)
	}
	return out
}
