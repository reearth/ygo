// Package cluster defines the relay abstraction that lets multiple ygo
// WebSocket server instances share a single logical document across processes.
//
// The model is a publish/subscribe fan-out keyed by room name. Each server
// node attaches a Relay; when a document or awareness change is committed
// locally, the node Publishes an Outbound event. The relay delivers it to
// every other node, which Injects it into its own in-memory room via the Sink.
//
// # Echo guard
//
// To prevent an infinite loop (node A publishes → node B injects → B's local
// observer publishes the same change back → A injects → …), inbound updates are
// applied to the local CRDT doc / awareness with a sentinel origin value. The
// per-room observers that drive Publish compare the update's origin against that
// sentinel by pointer identity and drop matches. This mirrors how
// websocket.Server.Apply tags its own writes (see provider/websocket/inject.go).
// The sentinel never crosses the wire — Outbound.Origin is observer-local
// metadata only.
package cluster

import (
	"context"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
)

// Kind distinguishes a CRDT document update from an awareness (presence) update.
type Kind int

const (
	// KindSync is a CRDT document update: Data is a V1 update blob suitable
	// for crdt.ApplyUpdateV1 / websocket.Server.BroadcastUpdate.
	KindSync Kind = iota
	// KindAwareness is an awareness update: Data is an awareness update blob
	// suitable for awareness.Awareness.ApplyUpdate / EncodeUpdate.
	KindAwareness
)

// String returns a human-readable name for the kind.
func (k Kind) String() string {
	switch k {
	case KindSync:
		return "sync"
	case KindAwareness:
		return "awareness"
	default:
		return "unknown"
	}
}

// Outbound is a locally-originated change a node Publishes to the relay.
//
// Origin is the origin value observed on the local change; the publishing node
// uses it only to drop echoes (a change that itself arrived via Inject). It is
// NOT serialised and has no meaning on the receiving node.
type Outbound struct {
	Room   string
	Kind   Kind
	Data   []byte
	Origin any
}

// Inbound is a remote change the relay delivers to a node via Sink.Inject.
type Inbound struct {
	Room string
	Kind Kind
	Data []byte
}

// Sink is the node-local surface a Relay drives to apply remote changes. The
// concrete implementation is *websocket.Server, which satisfies this interface
// directly (Inject, Rooms, GetAwareness, GetDoc).
type Sink interface {
	// Inject applies a remote change to the local room. For KindSync the data
	// is applied to the room's crdt.Doc with the relay sentinel origin and
	// rebroadcast to local peers; for KindAwareness it is merged into the
	// room's awareness.Awareness with the relay sentinel origin.
	Inject(ctx context.Context, in Inbound) error
	// Rooms returns the names of rooms currently resident on this node.
	Rooms() []string
	// GetAwareness returns the room's awareness state, or (nil,false) if the
	// room is not resident.
	GetAwareness(room string) (*awareness.Awareness, bool)
	// GetDoc returns the room's document, or nil if the room is not resident.
	GetDoc(room string) *crdt.Doc
}

// Relay is the cross-process transport. A node Publishes local changes and,
// after Start, receives remote changes which it applies via the Sink.
type Relay interface {
	// Publish broadcasts a locally-originated change to all other nodes. It is
	// the caller's responsibility (the provider wiring) to drop changes whose
	// Origin is the relay sentinel before calling Publish.
	Publish(ctx context.Context, out Outbound) error
	// Start binds a Sink for one node and begins delivering inbound changes to
	// it. Each node (each Server) calls Start once; a relay shared across
	// multiple nodes is Started once per node (a shared MemRelay therefore sees
	// multiple Start calls). The supplied ctx governs that node's delivery
	// lifetime; cancelling it (or calling Close) stops delivery.
	Start(ctx context.Context, sink Sink) error
	// RoomActivated tells the relay a room became resident on this node, so it
	// may begin subscribing to / delivering that room's traffic. Idempotent.
	RoomActivated(room string)
	// RoomDeactivated tells the relay a room is no longer resident on this
	// node. Idempotent.
	RoomDeactivated(room string)
	// Close stops the relay and releases its resources. After Close, Publish
	// returns ErrRelayClosed and no further inbound changes are delivered.
	Close() error
}
