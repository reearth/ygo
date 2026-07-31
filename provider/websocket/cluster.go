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

// relayOriginSentinel is the concrete type of the origin value AttachRelay
// stamps on every relay-injected doc/awareness change (see s.relaySentinel).
// registerRelayObservers' echo guard compares an update's origin against this
// value by == to tell "this arrived via the relay, don't re-publish it" apart
// from "this is a local change, publish it".
//
// This type MUST stay a non-zero-size struct (the `_ byte` field is load-
// bearing — do not remove it, and do not "simplify" this back to `struct{}`).
// Go's size-and-alignment guarantee lets the runtime satisfy every
// *zero-size* allocation from the same backing address (runtime.zerobase),
// so two unrelated `new(struct{})` calls anywhere in the process produce
// pointers that compare == to each other even though nothing about them is
// actually the same value. That is exactly what happened here pre-fix: this
// sentinel and inject.go's applyOriginSentinel (Server.Apply's own per-call
// origin, also once a bare `new(struct{})`) both collapsed onto zerobase and
// aliased. Every Server.Apply write was then misidentified by THIS echo guard
// as a self-echo of a relay-injected change and silently dropped before ever
// reaching enqueueRelayOutbound — permanent, silent cross-node divergence for
// any Apply call on a server with a relay attached. Giving each sentinel its
// own named, non-zero-size type removes the aliasing risk structurally (each
// instance gets its own heap allocation) AND belt-and-braces it with a
// distinct dynamic type, since `any` equality compares dynamic type before
// value.
type relayOriginSentinel struct{ _ byte }

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

	sentinel := &relayOriginSentinel{}
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

// ensureRelayLane creates and starts a FRESH outbound lane for room, for the
// exclusive use of the room instance calling it, and returns it. Called
// exactly once per room instance from registerRelayObservers (room
// creation), NEVER from the observer hot path: creating a lane takes the
// write lock and starts a goroutine, and the observer runs on the Transact
// commit path. Returns nil when no relay is attached.
//
// This does NOT reuse whatever is already in Server.relayLanes[room] — see
// room.relayLane's doc for why. If an entry is already there, it can only be
// a predecessor room instance's lane that has not yet been retired (room
// eviction removes the room from Server.rooms before it retires the lane —
// see peer.go's handleDisconnect / idle_sweep.go's evictIdleRoom — so a
// reconnect can create a brand new instance for the same name in that
// window). This function always installs its OWN fresh lane in that slot and
// retires the stale predecessor by closing its done channel — never by
// draining it directly, which would let two goroutines call relay.Publish
// for the same room concurrently (see relayLaneWorker's doc). The
// predecessor's own worker goroutine observes its done close and performs
// its own final drain before exiting, exactly as it would from its own
// stopRelayLane call.
//
// stopRelayLane and this function race safely on which one actually retires
// a given lane: both resolve it via a single relayLanesMu-guarded identity
// check against Server.relayLanes[room], so whichever one's critical section
// runs first "wins" the delete/replace and is the one that closes that
// lane's done; the other observes the map entry no longer matches and skips
// closing (avoiding a double-close panic).
func (s *Server) ensureRelayLane(room string) *relayRoomLane {
	s.relayLanesMu.Lock()
	if s.relayLanes == nil {
		s.relayLanesMu.Unlock()
		return nil // no relay attached
	}
	prev, hadPrev := s.relayLanes[room]
	l := &relayRoomLane{lane: relaylane.New(0), done: make(chan struct{})}
	s.relayLanes[room] = l
	s.relayLanesMu.Unlock()

	if hadPrev {
		close(prev.done)
	}
	go s.relayLaneWorker(s.relayCtx, room, l)
	return l
}

// relayLaneFor looks up a room's outbound lane BY NAME. Read-only, so the
// commit path never contends with lane creation or teardown. Returns nil
// when no relay is attached or the room has no lane yet.
//
// By name, not by room instance: the map entry for a given name always
// reflects whichever room instance most recently called ensureRelayLane, so
// this is correct across the room-eviction-then-reconnect transition
// (ensureRelayLane's identity-checked handoff guarantees the map is never
// stuck on a stale predecessor — see its doc). An observer callback for an
// evicted predecessor instance that is still (rarely) in flight during that
// transition will therefore route its Push onto the SUCCESSOR's lane rather
// than finding nil — which is correct, since both instances represent the
// same logical room and the relay only cares about {Room, Kind, Data}, not
// which room-instance produced it.
//
// The one race this can NOT close: a caller can obtain a non-nil
// *relayRoomLane here and then be descheduled before calling Push, for long
// enough that BOTH (a) that exact lane gets retired (ensureRelayLane's or
// stopRelayLane's identity-checked handoff — see their docs) AND (b) that
// lane's worker performs its final drain (relayLaneWorker's l.done case) and
// returns — all before the delayed Push executes. That Push then lands on an
// abandoned lane nobody will ever drain again, and is lost with no counter
// incremented. This mirrors the one accepted race documented on
// cluster/redis's Relay (see its `retired` field doc) and is not solvable
// without either blocking this lookup against teardown or keeping every
// retired lane around forever.
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

// stopRelayLane retires l, the specific outbound lane the caller's room
// instance created via ensureRelayLane (room.relayLane; nil is a valid no-op
// input for "no relay was attached"). It does NOT drain the lane itself —
// relayLaneWorker is this lane's sole consumer for its entire life, and
// draining from this goroutine too would let two goroutines call
// relay.Publish for the same room concurrently. Instead this only removes
// the lane from the map (so no new Push can find it) and signals the worker
// to stop; the worker's own l.done case performs the final drain before it
// returns, so whatever is queued still reaches Publish rather than being
// silently discarded.
//
// Deliberate deviation from a naive "check Empty() and drain here" teardown:
// the inbound side (cluster/redis's stopWorker / runRoomWorker) already
// learned this the hard way — its stopWorker doc explains the same
// worker-does-the-final-drain shape for the identical reason.
//
// l is retired ONLY if Server.relayLanes[room] still equals it: room
// eviction removes the room from Server.rooms before this function ever
// runs (see peer.go / idle_sweep.go), so a successor room instance can
// already have been created and called ensureRelayLane for the same name in
// the interim — and ensureRelayLane's own identity-checked handoff would
// then already have retired l on this call's behalf (see its doc). Deleting
// or closing done unconditionally here, by name alone, would either silently
// resurrect an already-retired predecessor's map entry or — worse — rip out
// a live successor's lane and kill its worker. See relayLaneFor's doc for the
// one narrower race this identity check does not (and cannot) close.
func (s *Server) stopRelayLane(room string, l *relayRoomLane) {
	if l == nil {
		return // no relay was attached for this room instance
	}
	s.relayLanesMu.Lock()
	cur, ok := s.relayLanes[room]
	stillCurrent := ok && cur == l
	if stillCurrent {
		delete(s.relayLanes, room)
	}
	s.relayLanesMu.Unlock() // write side: teardown, not the commit path
	if !stillCurrent {
		// A successor's ensureRelayLane already displaced and retired l.
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
	// during room creation, NOT on the commit path. Remember exactly which
	// lane THIS room instance owns (r.relayLane's doc) so teardownRelayRoom
	// can retire it by identity rather than by name alone.
	relayLane := s.ensureRelayLane(name)

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
	r.relayLane = relayLane
	r.mu.Unlock()
}

// enqueueRelayOutbound is the observer-side hand-off to the room's outbound
// worker. The observer runs on the CRDT commit path (the Transact caller's
// goroutine).
//
// The commit path never BLOCKS here: Lane.Push always returns without
// waiting on anything. It is NOT merge-free, though — an earlier version of
// this comment claimed merging "happens on the lane's worker instead", which
// is wrong: once the queued KindSync backlog exceeds relaylane.DefaultCap
// (64) entries, Push collapses the whole backlog into one blob via
// crdt.MergeUpdatesV1 synchronously, on THIS goroutine, before returning
// (see relaylane.Lane.Push / collapseLocked). So under a sustained wedge —
// this room's worker parked in a slow Publish — the commit path pays a
// bounded, amortized cost: roughly one merge per DefaultCap pushes, not one
// per push. That cost is real but small and infrequent, not a stall; #184
// tracks MergeUpdatesV1 itself as a hot path worth optimizing, which would
// directly shrink it further. The alternative — capping only on the Take*
// (consumer) side and never on Push — would trade this bounded latency for
// UNBOUNDED memory growth on a wedged room, which is worse.
//
// On a saturated lane the backlog is coalesced (merged), never silently
// dropped: an update lost here is NOT recoverable in general. The old
// comment (from before per-room lanes existed) claimed "peers reconcile via
// sync step 1/2", but reconciliation only happens when a room is reloaded,
// and a hot room (always at least one client) is never reloaded — so a
// dropped update would park every later edit from that client on the peer
// node. Coalescing avoids that. A hard drop remains possible only if the
// merge itself repeatedly fails; it is counted (relaylane.Stats.HardDrops)
// and surfaced via RelayStats (Task 6).
//
// relayLaneFor can legitimately return nil here: no relay is attached, or —
// see relayLaneFor's and ensureRelayLane's docs for the full detail — this
// observer callback is a genuine straggler that lost its race against this
// exact lane's retirement and drain. That is counted as a drop too, for the
// same reason — it is not recoverable in general — rather than silently
// discarded or treated as a bug.
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
	relayLane := r.relayLane
	r.relayLane = nil
	r.mu.Unlock()
	for _, u := range unsubs {
		u()
	}
	// Pass THIS instance's own remembered lane, not name alone: r.relayLane's
	// doc explains why a name-only lookup here would be unsafe across the
	// evict-then-reconnect window.
	s.stopRelayLane(name, relayLane)
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
