// Cross-process clustering: attach a cluster.Relay so multiple Server nodes
// share a single logical document/awareness state per room. See cluster_hooks.go
// for the GetAwareness / Rooms accessors used by the cluster.Sink contract, and
// the cluster package for the Relay/Sink interfaces and the reference MemRelay.
package websocket

import (
	"context"
	"errors"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// clusterRelay is the subset of cluster.Relay the Server drives. Declaring it
// here (rather than referencing cluster.Relay in server.go) keeps server.go free
// of a cluster import; the field is satisfied by any cluster.Relay.
type clusterRelay interface {
	Publish(ctx context.Context, out cluster.Outbound) error
	Start(ctx context.Context, sink cluster.Sink) error
	RoomActivated(room string)
	RoomDeactivated(room string)
	Close() error
}

// relayOutbound is the queue element drained by the relay worker. It is a local
// alias for cluster.Outbound so server.go can hold the chan field (s.relayOut)
// without importing the cluster package — the same import-discipline reason the
// clusterRelay interface lives here rather than in server.go.
type relayOutbound = cluster.Outbound

// ErrRelayAlreadyAttached is returned by AttachRelay if a relay is already set.
var ErrRelayAlreadyAttached = errors.New("ygo/websocket: relay already attached")

// ErrNilRelay is returned by AttachRelay when passed a nil relay.
var ErrNilRelay = errors.New("ygo/websocket: nil relay")

// Compile-time check: *Server satisfies cluster.Sink.
var _ cluster.Sink = (*Server)(nil)

// AttachRelay binds a cluster.Relay to this server so that local document and
// awareness changes are mirrored to other server nodes, and remote changes are
// injected into local rooms. It must be called once, before the server begins
// serving connections; once a relay is attached a second call returns
// ErrRelayAlreadyAttached.
//
// AttachRelay starts the relay (relay.Start(ctx, s)) with a context that is
// cancelled when Server.Shutdown is called. If relay.Start returns an error the
// server is left UNATTACHED (s.relay stays nil) and the call may be retried —
// it does not latch a partial attach. Rooms that become resident after attach
// are wired automatically (doc.OnUpdate + awareness.OnChange → a bounded
// outbound queue drained by a worker → Publish); the echo guard drops changes
// whose origin is the relay sentinel.
//
// Relay lifetime: the CALLER owns the relay and must Close() it once every
// attached server is done with it. Server.Shutdown only stops THIS server's
// delivery (it cancels the relay context); it does NOT Close the relay, because
// a single relay is commonly shared across multiple in-process Servers (the
// documented MemRelay pattern) and Closing it would stop delivery for all of
// them.
func (s *Server) AttachRelay(r cluster.Relay) error {
	if r == nil {
		return ErrNilRelay
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	if s.relay != nil {
		return ErrRelayAlreadyAttached
	}

	sentinel := new(struct{})
	ctx, cancel := context.WithCancel(context.Background())
	// Start before committing any state: a Start failure must leave the server
	// unattached and retryable.
	if err := r.Start(ctx, s); err != nil {
		cancel()
		return err
	}

	s.relaySentinel = sentinel
	s.relayCtx, s.relayCancel = ctx, cancel
	// Bounded outbound queue + worker (FIX B): CRDT observers enqueue
	// non-blockingly so the commit path never stalls on a slow relay; the
	// worker drives Publish and logs failures.
	s.relayOut = make(chan relayOutbound, 1024)
	go s.relayWorker(ctx, s.relayOut)
	// Publish s.relay last: anything gated on s.relay != nil (getOrCreateRoom
	// registering observers) then always sees a ready outbound queue + worker.
	s.relay = r
	return nil
}

// relayWorker drains the bounded outbound queue and drives relay.Publish. It is
// the only goroutine that calls Publish, so a blocking relay back-pressures only
// the worker (and, via a full queue, causes the observers to drop) — never the
// CRDT commit path. The worker exits when relayCtx is cancelled (Shutdown).
func (s *Server) relayWorker(ctx context.Context, out <-chan relayOutbound) {
	for {
		select {
		case <-ctx.Done():
			return
		case ob := <-out:
			if err := s.relay.Publish(ctx, ob); err != nil {
				s.log().Warn("relay publish failed", "room", ob.Room, "err", err)
			}
		}
	}
}

// registerRelayObservers wires doc.OnUpdate and awareness.OnChange for a room so
// local (non-sentinel-origin) changes are published to the relay. Must be called
// with s.rmu.Lock held (from getOrCreateRoom). The unsubscribe functions are
// stored on the room and invoked by teardownRelayRoom.
func (s *Server) registerRelayObservers(r *room, name string) {
	sentinel := s.relaySentinel

	unsubDoc := r.doc.OnUpdate(func(update []byte, origin any) {
		if origin == sentinel {
			return // echo guard: this change arrived via the relay
		}
		// Copy the update: the slice handed to OnUpdate observers may alias
		// internal buffers, and the observer must not block — it enqueues onto
		// the bounded outbound queue and returns, so the data may be read by the
		// worker after this observer returns.
		cp := append([]byte(nil), update...)
		s.enqueueRelayOutbound(name, cluster.Outbound{
			Room: name, Kind: cluster.KindSync, Data: cp, Origin: origin,
		})
	})

	unsubAw := r.awareness.OnChange(func(ev awareness.ChangeEvent) {
		if ev.Origin == sentinel {
			return // echo guard
		}
		ids := changedAwarenessIDs(ev)
		if len(ids) == 0 {
			return
		}
		data := r.awareness.EncodeUpdate(ids)
		s.enqueueRelayOutbound(name, cluster.Outbound{
			Room: name, Kind: cluster.KindAwareness, Data: data, Origin: ev.Origin,
		})
	})

	r.mu.Lock()
	r.relayUnsub = append(r.relayUnsub, unsubDoc, unsubAw)
	r.mu.Unlock()
}

// enqueueRelayOutbound is the observer-side, NON-BLOCKING hand-off to the relay
// worker. The provider buffers outbound events on a bounded queue and drives
// Publish from a dedicated worker goroutine, so Publish may block only that
// worker, never the CRDT commit path the observer runs on (the Transact caller
// goroutine). On sustained overflow the queue drops the event and bumps a
// counter — losing a relay echo is recoverable (peers reconcile via sync
// step 1/2), stalling every local commit is not.
func (s *Server) enqueueRelayOutbound(name string, out cluster.Outbound) {
	select {
	case s.relayOut <- out:
	default:
		s.relayDropped.Add(1)
		s.log().Debug("relay outbound queue full, dropping", "room", name)
	}
}

// changedAwarenessIDs returns the union of added/updated/removed client IDs.
func changedAwarenessIDs(ev awareness.ChangeEvent) []uint64 {
	ids := make([]uint64, 0, len(ev.Added)+len(ev.Updated)+len(ev.Removed))
	ids = append(ids, ev.Added...)
	ids = append(ids, ev.Updated...)
	ids = append(ids, ev.Removed...)
	return ids
}

// teardownRelayRoom unsubscribes the relay observers for an evicted room and
// notifies the relay. Safe to call when no relay is attached (no-op) and
// idempotent per room (relayUnsub is cleared after firing).
func (s *Server) teardownRelayRoom(r *room, name string) {
	if s.relay == nil {
		return
	}
	r.mu.Lock()
	unsubs := r.relayUnsub
	r.relayUnsub = nil
	r.mu.Unlock()
	for _, u := range unsubs {
		u()
	}
	s.relay.RoomDeactivated(name)
}

// Inject applies a remote change delivered by the relay. It satisfies
// cluster.Sink. KindSync updates are applied to the room's doc with the relay
// sentinel origin (so the local doc.OnUpdate observer does NOT re-publish them)
// and rebroadcast to local peers via BroadcastUpdate. KindAwareness updates are
// merged into the room's awareness with the sentinel origin; the awareness
// fan-out to peers is driven by the awareness OnChange path the server already
// runs for local peers — but inbound merges fire OnChange with the sentinel
// origin, which the relay observer drops, so we additionally fan the awareness
// update out to local peers here.
//
// Inject auto-creates the room if it is not yet resident, so a node that has no
// local peers for a room still materialises the converged state (matching how a
// fresh peer would receive it via sync step-2 once it connects).
func (s *Server) Inject(ctx context.Context, in cluster.Inbound) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.shutdownCh:
		return ErrServerShutdown
	default:
	}
	if !isValidRoomName(in.Room) {
		return ErrInvalidRoomName
	}

	switch in.Kind {
	case cluster.KindSync:
		rm, err := s.getOrCreateRoom(ctx, in.Room)
		if err != nil {
			return err
		}
		if err := crdt.ApplyUpdateV1(rm.doc, in.Data, s.relaySentinel); err != nil {
			return err
		}
		// Rebroadcast to locally-connected peers. broadcastUpdate re-validates
		// and frames; it does not re-apply to the doc (already applied above).
		// fireHook=false: OnInject governs LOCALLY-originated writes; firing it
		// here could veto the fan-out after the doc was already mutated above,
		// silently diverging this node from the cluster (FIX H).
		return s.broadcastUpdate(ctx, in.Room, in.Data, false)

	case cluster.KindAwareness:
		rm, err := s.getOrCreateRoom(ctx, in.Room)
		if err != nil {
			return err
		}
		if err := rm.awareness.ApplyUpdate(in.Data, s.relaySentinel); err != nil {
			return err
		}
		// Fan the awareness update out to local peers (the OnChange-driven relay
		// observer dropped it as a sentinel echo, so peers won't otherwise see it).
		s.broadcastAwarenessToPeers(rm, in.Data)
		return nil

	default:
		return nil
	}
}

// broadcastAwarenessToPeers sends a raw awareness update to every peer in the
// room (no exclusions — the originating peer is on another node).
func (s *Server) broadcastAwarenessToPeers(rm *room, awBytes []byte) {
	rm.mu.Lock()
	targets := make([]*peer, 0, len(rm.peers))
	for p := range rm.peers {
		targets = append(targets, p)
	}
	rm.mu.Unlock()
	for _, p := range targets {
		p.sendAwareness(awBytes)
	}
}
