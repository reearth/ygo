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
	//
	// Inject MAY BE CALLED CONCURRENTLY for distinct rooms — a relay is not
	// required to deliver every room from a single goroutine, and every Sink
	// implementation must be safe for that regardless of which relay ends up
	// attached. *websocket.Server is (it is the same path concurrent
	// connections already take). Calls for the SAME room must still be
	// serialised and delivered in publish order.
	//
	// This is a permission on the interface, not a guarantee every relay
	// exercises: cluster/redis's Relay takes advantage of it — it delivers
	// each room on its own goroutine so one slow room cannot stall inbound
	// delivery for the others (#187) — but MemRelay does not (yet): it still
	// delivers every room from one goroutine per node, so a Sink attached
	// only to a MemRelay does not get that isolation from MemRelay itself.
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
	//
	// Publish MAY BE CALLED CONCURRENTLY for distinct rooms — the provider
	// (provider/websocket) drives it from one worker goroutine per room, not
	// one per server, so calls for different rooms are expected to overlap.
	// Every Relay implementation must be safe for that.
	//
	// The contract imposes NO per-room ordering: implementations must not
	// rely on receiving a room's publishes in any particular order relative
	// to each other, and must tolerate TWO CONCURRENT Publish calls for the
	// SAME room. That same-room overlap is narrow and short-lived — it can
	// only happen across a room's eviction/reload handoff, where a
	// predecessor lane's final drain briefly overlaps with the successor
	// lane's worker publishing for the same room name — but it is a real
	// possibility a third-party Relay must not assume away (e.g. by keeping
	// an unlocked per-room sequence counter, or appending to an
	// unsynchronised buffer). This was reviewed and accepted as benign at the
	// provider level because the Relay contract imposes no per-room ordering,
	// KindSync payloads are commutative/idempotent V1 update blobs regardless
	// of arrival order, and a stale KindAwareness payload is dropped by the
	// receiving Awareness's own per-client clock gate — but a Relay
	// implementation still needs to be safe for the concurrent calls
	// themselves, independent of that payload-level reasoning. Both shipped
	// relays already are: MemRelay.Publish snapshots the node list under its
	// own mutex, releases it, then sends on per-node channels; cluster/redis's
	// Publish deliberately takes no lock at all and uses atomics plus
	// channels.
	//
	// Publish MUST return promptly once ctx is cancelled (returning ctx's
	// error for whatever it could not deliver). websocket.Server.Shutdown
	// relies on this to unwedge a blocked Publish after its own deadline: it
	// cancels the relay context and then joins the lane workers, counting
	// every payload an aborted Publish abandons in RelayStats().Dropped
	// (#202). A Publish that ignores cancellation stalls that join and
	// leaves its worker goroutine running past Shutdown. Both shipped relays
	// conform: MemRelay selects on ctx around its per-node channel sends,
	// and cluster/redis's Publish selects on ctx around the hand-off to its
	// publisher goroutine.
	Publish(ctx context.Context, out Outbound) error
	// Start binds a Sink for one node and begins delivering inbound changes to
	// it. Each node (each Server) calls Start once; a relay shared across
	// multiple nodes is Started once per node (a shared MemRelay therefore sees
	// multiple Start calls). The supplied ctx governs that node's delivery
	// lifetime; cancelling it (or calling Close) stops delivery.
	Start(ctx context.Context, sink Sink) error
	// RoomActivated tells the relay a room became resident on this node, so it
	// may begin subscribing to / delivering that room's traffic. Idempotent.
	//
	// Implementations MUST tolerate a successor room instance activating the
	// same name before a predecessor's RoomDeactivated for that name has been
	// called. During a room's eviction/reload window, the websocket provider's
	// teardown and lookup paths are decoupled enough that a fresh room
	// instance can call RoomActivated(name) while the outgoing instance's
	// teardown — which calls RoomDeactivated(name) — is still in flight, in
	// either order. A Relay that reference-counts activations per name (both
	// shipped relays do: cluster/redis via its activeRooms counter, MemRelay
	// trivially since both calls are no-ops) rides this out correctly; one
	// that treats RoomDeactivated as an unconditional unsubscribe would drop
	// a live successor room's subscription.
	RoomActivated(room string)
	// RoomDeactivated tells the relay a room is no longer resident on this
	// node. Idempotent. See RoomActivated's doc for the activation-overlap
	// requirement this call is one half of.
	//
	// Implementations MUST tolerate a Publish for this room arriving AFTER
	// RoomDeactivated has returned: the provider's teardown stops the room's
	// outbound lane asynchronously and calls RoomDeactivated right away,
	// without waiting for the lane worker's own final drain — which runs on
	// its own goroutine and can still call Publish — to finish. A Relay that
	// releases per-room PUBLISH-side state (a producer handle, a partition
	// assignment, a stream key) here would drop that trailing update. Both
	// shipped relays are unaffected: cluster/redis's RoomDeactivated only
	// UNSUBSCRIBEs, which is inbound-only and never consulted by Publish;
	// MemRelay no-ops both calls.
	RoomDeactivated(room string)
	// Close stops the relay and releases its resources. After Close, Publish
	// returns ErrRelayClosed and no further inbound changes are delivered.
	Close() error
}
