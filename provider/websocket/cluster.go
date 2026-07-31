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
	"github.com/reearth/ygo/internal/relaylane"
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

// relayRoomLane is one room's outbound queue plus the worker draining it.
// Declared here rather than in server.go for the same import-discipline
// reason the clusterRelay interface is: server.go stays free of a direct
// cluster import.
type relayRoomLane struct {
	lane *relaylane.Lane
	done chan struct{}
}

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
// are wired automatically (doc.OnUpdate + awareness.OnUpdate → that room's own
// outbound lane, drained by its own worker → Publish), so a relay that is slow
// for one room cannot stall publishing for any other room (#187); the echo
// guard drops changes whose origin is the relay sentinel.
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
	// Per-room outbound lanes (#187): each room's Publish is driven by its own
	// worker, so a relay that is slow for one room cannot stall any other
	// room's publishes — and never the CRDT commit path.
	s.relayLanesMu.Lock()
	s.relayLanes = make(map[string]*relayRoomLane)
	s.relayLanesMu.Unlock()
	// Publish s.relay last: anything gated on s.relay != nil (getOrCreateRoom
	// registering observers) then always sees a ready relayLanes map.
	s.relay = r
	return nil
}

// ensureRelayLane creates and starts the outbound lane for room if it does not
// exist. Called from registerRelayObservers (room creation), NEVER from the
// observer hot path: creating a lane takes the write lock and starts a
// goroutine, and the observer runs on the Transact commit path.
func (s *Server) ensureRelayLane(room string) {
	s.relayLanesMu.Lock()
	defer s.relayLanesMu.Unlock()
	if s.relayLanes == nil {
		return // no relay attached
	}
	if _, ok := s.relayLanes[room]; ok {
		return
	}
	l := &relayRoomLane{lane: relaylane.New(0), done: make(chan struct{})}
	s.relayLanes[room] = l
	go s.relayLaneWorker(s.relayCtx, room, l)
}

// relayLaneFor looks up a room's outbound lane. Read-only, so the commit path
// never contends with lane creation or teardown. Returns nil when no relay is
// attached or the room has no lane yet.
//
// A caller can hold onto the returned *relayRoomLane after stopRelayLane has
// already removed it from the map — an observer callback in flight on another
// goroutine looked this up just before teardown. Pushing onto that stale
// reference is harmless in the common case (the worker's final drain, see
// relayLaneWorker's l.done case, still picks it up) but there is one narrow,
// accepted window: a Push landing after that final drain has already
// executed and the worker has returned is lost with no counter incremented.
// This mirrors the one accepted race documented on cluster/redis's Relay
// (see its `retired` field doc) and is not solvable without either blocking
// this lookup against teardown or keeping retired lanes around forever.
func (s *Server) relayLaneFor(room string) *relayRoomLane {
	s.relayLanesMu.RLock()
	defer s.relayLanesMu.RUnlock()
	return s.relayLanes[room]
}

// relayLaneWorker drives relay.Publish for one room. It drains the lane
// greedily, so a backlog is merged into a single Publish rather than N — which
// both cuts Publish calls and makes a full lane rare.
//
// The l.done case performs one final drainRelayLane before returning, rather
// than returning immediately. This worker is the lane's ONLY consumer for its
// entire life — stopRelayLane deliberately does not drain the lane itself,
// precisely so nothing else ever calls relay.Publish for this room
// concurrently with this goroutine (see stopRelayLane's doc for why that
// matters: two drainers can never double-deliver the same payload — Lane's
// Take* are mutex-guarded — but they could each independently and
// concurrently call relay.Publish for the same room, which is an invariant
// this design does not want to depend on relay implementations tolerating).
func (s *Server) relayLaneWorker(ctx context.Context, room string, l *relayRoomLane) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.done:
			s.drainRelayLane(ctx, room, l)
			return
		case <-l.lane.Signal():
			s.drainRelayLane(ctx, room, l)
		}
	}
}

func (s *Server) drainRelayLane(ctx context.Context, room string, l *relayRoomLane) {
	for {
		if data, ok := l.lane.TakeSync(); ok {
			s.publishRelay(ctx, room, cluster.KindSync, data)
			continue
		}
		if data, ok := l.lane.TakeAwareness(); ok {
			s.publishRelay(ctx, room, cluster.KindAwareness, data)
			continue
		}
		return
	}
}

func (s *Server) publishRelay(ctx context.Context, room string, kind cluster.Kind, data []byte) {
	if err := s.relay.Publish(ctx, cluster.Outbound{
		Room: room, Kind: kind, Data: data,
	}); err != nil {
		s.log().Warn("relay publish failed", "room", room, "kind", kind, "err", err)
	}
}

// stopRelayLane retires a room's outbound lane. It does NOT drain the lane
// itself — relayLaneWorker is this lane's sole consumer for its entire life,
// and draining from this goroutine too would let two goroutines call
// relay.Publish for the same room concurrently. Instead this only removes the
// lane from the map (so no new Push can find it) and signals the worker to
// stop; the worker's own l.done case performs the final drain before it
// returns, so whatever is queued still reaches Publish rather than being
// silently discarded.
//
// Deliberate deviation from a naive "check Empty() and drain here" teardown:
// the inbound side (cluster/redis's stopWorker / runRoomWorker) already
// learned this the hard way — its stopWorker doc explains the same
// worker-does-the-final-drain shape for the identical reason. See
// relayLaneFor's doc for the residual race this still leaves (a Push that
// lands on a stale lane reference after this goroutine's final drain has
// already run).
func (s *Server) stopRelayLane(room string) {
	s.relayLanesMu.Lock()
	l, ok := s.relayLanes[room]
	if ok {
		delete(s.relayLanes, room)
	}
	s.relayLanesMu.Unlock() // write side: teardown, not the commit path
	if !ok {
		return
	}
	close(l.done)
}

// registerRelayObservers wires doc.OnUpdate and awareness.OnUpdate for a room so
// local (non-sentinel-origin) changes are published to the relay. The awareness
// side deliberately subscribes via OnUpdate rather than OnChange: OnUpdate fires
// for every applied entry, including content-identical heartbeat re-emits, so a
// heartbeat still gets relayed cross-node. Using OnChange here would drop
// heartbeats from the relay (OnChange only fires on content changes), leaving a
// remote node's view of this client's liveness stale and causing it to falsely
// expire a still-alive client. Must be called with s.rmu.Lock held (from
// getOrCreateRoomLocked). The unsubscribe functions are stored on the room and
// invoked by teardownRelayRoom.
func (s *Server) registerRelayObservers(r *room, name string) {
	sentinel := s.relaySentinel
	// Create the outbound lane before the observers that feed it. This runs
	// during room creation, NOT on the commit path.
	s.ensureRelayLane(name)

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

	unsubAw := r.awareness.OnUpdate(func(ev awareness.UpdateEvent) {
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

// enqueueRelayOutbound is the observer-side, NON-BLOCKING hand-off to the
// room's outbound worker. The observer runs on the CRDT commit path (the
// Transact caller's goroutine), so this must never block and must never merge
// here — merging happens on the lane's worker instead.
//
// On a saturated lane the queued KindSync backlog is coalesced (merged), not
// dropped: an update lost here is NOT recoverable in general. The old comment
// claimed "peers reconcile via sync step 1/2", but reconciliation only happens
// when a room is reloaded, and a hot room (always at least one client) is
// never reloaded — so a dropped update parks every later edit from that client
// on the peer node. Coalescing avoids that. A hard drop remains possible only
// if the merge itself fails; it is counted and surfaced via RelayStats.
//
// relayLaneFor can legitimately return nil here: registerRelayObservers
// creates the lane before wiring these observers, but stopRelayLane can retire
// a room's lane while an observer callback for that same room is already
// in-flight on another goroutine (a straggler). That is counted as a drop
// too, for the same reason — it is not recoverable in general — rather than
// silently discarded or treated as a bug.
func (s *Server) enqueueRelayOutbound(name string, out cluster.Outbound) {
	l := s.relayLaneFor(name)
	if l == nil {
		s.relayDropped.Add(1)
		s.log().Debug("relay outbound: no lane for room, dropping", "room", name)
		return // no relay attached, or the room's lane was already retired
	}
	l.lane.Push(out.Kind, out.Data)
}

// changedAwarenessIDs returns the union of added/updated/removed client IDs.
func changedAwarenessIDs(ev awareness.UpdateEvent) []uint64 {
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
	s.stopRelayLane(name)
	s.relay.RoomDeactivated(name)
}

// Inject applies a remote change delivered by the relay. It satisfies
// cluster.Sink. KindSync updates are applied to the room's doc with the relay
// sentinel origin (so the local doc.OnUpdate observer does NOT re-publish them)
// and rebroadcast to local peers via BroadcastUpdate. KindAwareness updates are
// merged into the room's awareness with the sentinel origin; the awareness
// fan-out to peers is driven by the awareness OnUpdate path the server already
// runs for local peers — but inbound merges fire OnUpdate with the sentinel
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
		rm, _, err := s.getOrCreateRoom(ctx, in.Room)
		if err != nil {
			return err
		}
		// Balance the inflight++ from getOrCreateRoom: this call uses rm
		// synchronously and then returns, so a deferred release is correct
		// (#193 review). Also protects rm from a concurrent eviction while
		// in use.
		defer s.releaseInflight(rm)
		rm.clearIdle() // #183: relay activity mutates immediately; no registration delay.
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
		rm, _, err := s.getOrCreateRoom(ctx, in.Room)
		if err != nil {
			return err
		}
		// Balance the inflight++ from getOrCreateRoom (#193 review); see the
		// KindSync case above.
		defer s.releaseInflight(rm)
		rm.clearIdle() // #183: relay activity mutates immediately; no registration delay.
		if err := rm.awareness.ApplyUpdate(in.Data, s.relaySentinel); err != nil {
			return err
		}
		// Fan the awareness update out to local peers (the OnUpdate-driven relay
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
