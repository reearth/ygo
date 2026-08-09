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
// draining it directly, which would let TWO GOROUTINES drain THE SAME LANE
// concurrently (relayLaneWorker's doc explains why that specific case is
// unwanted: it isn't about double-delivery, which Lane's mutex-guarded Take*
// already rules out, but about not depending on relay implementations
// tolerating concurrent Publish calls sharing one lane's backlog). The
// predecessor's own worker goroutine observes its done close and performs
// its own final drain before exiting, exactly as it would from its own
// stopRelayLane call.
//
// That final drain is this handoff's one accepted overlap: it calls
// relay.Publish(room, ...) for the OLD lane while the brand-new worker
// spawned below can already be calling relay.Publish for the SAME room name
// through the NEW lane — two different lanes, two different goroutines, one
// room string, briefly concurrent. This is safe, not merely tolerated,
// because cluster.Relay's contract imposes no per-room ordering or mutual
// exclusion across Publish calls: KindSync payloads are V1 update blobs,
// which are commutative and idempotent regardless of delivery order, and a
// stale KindAwareness payload arriving after a newer one is dropped by the
// receiving Awareness's own per-client clock gate (see awareness.go's
// ApplyUpdate), not by any ordering guarantee this package provides. An
// earlier version of this comment (and relayLaneWorker's/stopRelayLane's)
// asserted "exactly one goroutine Publishes per room" as an invariant this
// design upholds — that was wrong: it holds per LANE, never across this
// handoff's brief two-lane overlap for the same room name.
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
	// Register the worker on relayWG INSIDE the locked section: Shutdown's
	// retireRelayLanes sets s.relayLanes to nil under this same mutex before
	// its Waits start, so every Add is either strictly ordered before those
	// Waits or never happens at all (the nil check above already returned).
	s.relayWG.Add(1)
	if hadPrev {
		// This is the OTHER place (besides stopRelayLane) a lane can be
		// retired, so it needs the identical fold: without it, a predecessor
		// displaced here — e.g. exactly the reconnect-during-teardown handoff
		// TestRelayOutbound_SurvivesEvictionRace exercises — would have its
		// counters silently vanish from RelayStats() the moment it's replaced,
		// breaking the same monotonicity guarantee stopRelayLane's fold exists
		// to protect. See RelayStats' doc in this file and relayRetired's doc
		// in server.go.
		st := prev.lane.Stats()
		s.relayRetired.Coalesced += st.Coalesced
		s.relayRetired.AwarenessSuperseded += st.AwarenessSuperseded
		s.relayRetired.HardDrops += st.HardDrops
	}
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
// than returning immediately. This worker is l's ONLY consumer for its entire
// life — stopRelayLane deliberately does not drain l itself, precisely so
// nothing else ever drains l (and therefore calls relay.Publish with l's
// backlog) concurrently with this goroutine (see stopRelayLane's doc for why
// that matters: two drainers of the SAME lane could each independently and
// concurrently call relay.Publish, which is an invariant this design does not
// want to depend on relay implementations tolerating — even though they could
// never double-deliver the same payload, since Lane's Take* are mutex-
// guarded). This is a per-lane guarantee only: during the eviction handoff
// (ensureRelayLane's doc), a predecessor lane's final drain and a brand-new
// successor lane's worker CAN call relay.Publish for the same room name at
// the same time — that is a different, deliberately accepted overlap between
// two distinct lanes, not a violation of this one.
func (s *Server) relayLaneWorker(ctx context.Context, room string, l *relayRoomLane) {
	defer s.relayWG.Done()
	for {
		select {
		case <-ctx.Done():
			// The relay context is dead, so the backlog cannot be published —
			// but it must not vanish while Dropped reads zero (#202): count
			// every abandoned payload. In the ordinary Shutdown sequence this
			// case is rare (retireRelayLanes closes l.done and the final drain
			// runs under a still-live relay context first); it is reached when
			// Shutdown's ctx expired before the drain finished, or on a lane
			// still live at cancellation time.
			s.discardRelayLane(room, l)
			return
		case <-l.done:
			s.drainRelayLane(ctx, room, l)
			return
		case <-l.lane.Signal():
			s.drainRelayLane(ctx, room, l)
		}
	}
}

// discardRelayLane empties l without publishing, counting every abandoned
// payload in relayDropped so the loss is visible in RelayStats(). Only called
// once the relay context is cancelled — publishing is no longer possible.
func (s *Server) discardRelayLane(room string, l *relayRoomLane) {
	n := 0
	for {
		if _, ok := l.lane.TakeSync(); ok {
			n++
			continue
		}
		if _, ok := l.lane.TakeAwareness(); ok {
			n++
			continue
		}
		break
	}
	if n > 0 {
		s.relayDropped.Add(uint64(n))
		s.log().Warn("relay outbound: shutdown discarded undeliverable backlog",
			"room", room, "payloads", n)
	}
}

// retireRelayLanes retires EVERY remaining outbound lane at once and marks
// the lane table closed: called only from Shutdown, after peers are gone and
// the persistence drain is over. Setting s.relayLanes to nil (rather than
// deleting entries) makes ensureRelayLane refuse new lanes from this point on
// — it already treats a nil map as "no relay attached" — so no worker can be
// created after Shutdown's WaitGroup joins begin, and a straggling
// enqueueRelayOutbound finds no lane and counts its payload in Dropped
// instead of parking it where nothing will ever drain it. RelayStats() keeps
// reporting: it reads s.relayRetired (folded here) plus a range over the nil
// map, which is a no-op.
//
// Concurrency with the other two retirement sites is the same
// identity-under-mutex dance they play with each other: a stopRelayLane that
// got there first already removed its lane from the map (so it is not in
// this snapshot, and only that call closes its done); one that arrives after
// finds s.relayLanes[room] no longer matching (nil map) and skips its own
// close — never a double close.
func (s *Server) retireRelayLanes() {
	s.relayLanesMu.Lock()
	lanes := s.relayLanes
	s.relayLanes = nil
	for _, l := range lanes {
		st := l.lane.Stats()
		s.relayRetired.Coalesced += st.Coalesced
		s.relayRetired.AwarenessSuperseded += st.AwarenessSuperseded
		s.relayRetired.HardDrops += st.HardDrops
	}
	s.relayLanesMu.Unlock()
	for _, l := range lanes {
		close(l.done)
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
		// The payload was already taken off the lane, so a failed Publish
		// loses it — count it (#202). There is no retry path: KindSync blobs
		// are only recoverable via a room reload's sync step, which a hot
		// room never performs, so "logged but uncounted" would be exactly
		// the silent divergence RelayStats.Dropped exists to surface.
		s.relayDropped.Add(1)
		s.log().Warn("relay publish failed", "room", room, "kind", kind, "err", err)
	}
}

// stopRelayLane retires l, the specific outbound lane the caller's room
// instance created via ensureRelayLane (room.relayLane; nil is a valid no-op
// input for "no relay was attached"). It does NOT drain the lane itself —
// relayLaneWorker is l's sole consumer for its entire life, and draining from
// this goroutine too would let two goroutines drain l concurrently and each
// independently call relay.Publish with l's backlog. Instead this only
// removes the lane from the map (so no new Push can find it) and signals the
// worker to stop; the worker's own l.done case performs the final drain
// before it returns, so whatever is queued still reaches Publish rather than
// being silently discarded. (That final drain can itself overlap with a
// brand-new successor lane's Publish calls for the same room name — see
// ensureRelayLane's doc for why that specific, cross-lane overlap is safe.)
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
//
// Before dropping l, its current Coalesced/AwarenessSuperseded/HardDrops
// counters are folded into s.relayRetired (still under relayLanesMu) so
// RelayStats() keeps them after this lane is gone — otherwise an evicted
// room's counters would simply vanish from RelayStats(), letting its running
// totals go backwards. Mirrors cluster/redis's stopWorker (see its own doc
// for the identical reasoning and the one narrow, accepted race this leaves —
// RelayStats' doc in cluster.go has the outbound-side restatement).
func (s *Server) stopRelayLane(room string, l *relayRoomLane) {
	if l == nil {
		return // no relay was attached for this room instance
	}
	s.relayLanesMu.Lock()
	cur, ok := s.relayLanes[room]
	stillCurrent := ok && cur == l
	if stillCurrent {
		delete(s.relayLanes, room)
		st := l.lane.Stats()
		s.relayRetired.Coalesced += st.Coalesced
		s.relayRetired.AwarenessSuperseded += st.AwarenessSuperseded
		s.relayRetired.HardDrops += st.HardDrops
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
// expire a still-alive client. Called from loadRoom, AFTER the bootstrap
// decode but BEFORE close(r.ready), with s.rmu released (loadRoom runs off the
// global rooms lock — see server.go). The unsubscribe functions are stored on
// the room and invoked by teardownRelayRoom.
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
// bounded, amortized cost of roughly one merge per DefaultCap pushes, NOT one
// per push — but only while that merge keeps SUCCEEDING. collapseLocked
// leaves the backlog fully intact on a failed merge (it does not clear
// syncQ), so every push after that stays over cap and re-triggers
// collapseLocked again: from push cap+1 onward, a failing merge means one
// full MergeUpdatesV1 attempt PER PUSH, not amortized at all — and this does
// NOT end once the backlog reaches 2*cap; it continues indefinitely, one
// hard-drop per push thereafter, for as long as MergeUpdatesV1 keeps failing
// (see collapseLocked's own doc). This degenerate case requires
// MergeUpdatesV1 itself to be failing repeatedly — the same condition that
// produces HardDrops — so it is already the unhealthy path RelayStats.HardDrops
// exists to surface; the amortized-cost claim above describes the
// healthy, expected case. That cost is real but small and infrequent in the
// healthy case, not a stall; #184 tracks MergeUpdatesV1 itself as a hot path
// worth optimizing, which would directly shrink it further. The alternative —
// capping only on the Take* (consumer) side and never on Push — would trade
// this bounded latency for UNBOUNDED memory growth on a wedged room, which
// is worse.
//
// On a saturated lane the backlog is coalesced (merged), never silently
// dropped: an update lost here is NOT recoverable in general. The old
// comment (from before per-room lanes existed) claimed "peers reconcile via
// sync step 1/2", but reconciliation only happens when a room is reloaded,
// and a hot room (always at least one client) is never reloaded — so a
// dropped update would park every later edit from that client on the peer
// node. Coalescing avoids that. A hard drop remains possible only if the
// merge itself repeatedly fails; it is counted (relaylane.Stats.HardDrops)
// and surfaced via RelayStats.
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

// RelayStats is a point-in-time snapshot of this server's OUTBOUND relay
// health, summed across every room lane this server has ever had — live ones
// plus ones since retired (by stopRelayLane's ordinary teardown or
// ensureRelayLane's predecessor-displacement handoff; their final counts are
// folded into s.relayRetired before the lane is dropped — see relayRetired's
// doc in server.go), mirroring cluster/redis's Relay.Stats() / retired field
// for the identical reason. For the inbound side, see the relay adapter's own
// Stats (e.g. cluster/redis.Relay.Stats).
//
// Coalesced/AwarenessSuperseded/HardDrops must be kept in sync with
// relaylane.Stats — see RelayStats' construction in the method below, and
// TestPublicStatsCoverRelaylaneFields (cluster_redis_test.go), which reflects
// over both this type and cluster/redis.Stats and fails if either falls out
// of sync with relaylane.Stats's field set.
//
// Coalesced going non-zero means at least one room's publishes fell behind
// and were merged (lossless). Dropped or HardDrops going non-zero means
// updates were lost and peer nodes may be diverged — alert on both.
//
// Monotonicity: every field here is guaranteed to never decrease across the
// life of the Server — see RelayStats()'s doc for exactly why, and for the
// two accepted undercount gaps (never a decrease, just an occasional missed
// count) that mirror the inbound side's identical, accepted gaps.
type RelayStats struct {
	// Coalesced counts outbound KindSync updates absorbed into another blob
	// by a merge.
	Coalesced uint64
	// AwarenessSuperseded counts awareness blobs replaced before publish.
	// Benign: awareness is idempotent heartbeat state.
	AwarenessSuperseded uint64
	// HardDrops counts payloads lost because a merge failed on a saturated
	// lane. Should always be zero.
	HardDrops uint64
	// Dropped counts every payload that was lost after the commit path
	// handed it to the outbound side: discarded before reaching a lane (no
	// relay attached at enqueue time, or a genuine straggler that lost its
	// race against its lane's own retirement — see enqueueRelayOutbound's
	// doc), taken from a lane but abandoned because relay.Publish returned
	// an error (there is no retry path — see publishRelay), or still queued
	// in a lane when Shutdown had to give up on delivery (see
	// discardRelayLane). Should always be zero in a healthy deployment.
	//
	// Since the #202 fix there is no shutdown exception: Server.Shutdown
	// drains each lane under the still-live relay context before cancelling
	// it, and whatever it cannot deliver within its ctx budget is counted
	// here. Dropped and HardDrops both reading zero after a Shutdown means
	// nothing was lost.
	Dropped uint64
}

// RelayStats returns a snapshot of outbound relay counters, safe to call
// concurrently — including when no relay is attached (all zeroes) — and
// never touching the Transact commit path (it is a polled diagnostic; see
// the package invariant that nothing on doc.OnUpdate/awareness.OnUpdate may
// gain new work because of this method's existence).
//
// Guarantee: Coalesced/AwarenessSuperseded/HardDrops are MONOTONIC across
// sequential calls (never decrease) but not guaranteed EXACT — mirroring
// cluster/redis's Relay.Stats(), including its accepted gaps. Dropped is
// exact (a single atomic counter) and therefore also monotonic. There are
// TWO distinct sources of undercount, both benign (never an overcount, never
// a decrease relative to any total already returned):
//
//  1. (Narrow race) A straggling Push that lands on a lane by name
//     (relayLaneFor) after stopRelayLane/ensureRelayLane has already folded
//     that lane's counters into s.relayRetired and dropped it from
//     s.relayLanes contributes to the counters of a lane nothing will ever
//     read again.
//
//  2. (ROUTINE, not narrow — happens on every retirement of a lane with a
//     backlog) Both fold sites read l.lane.Stats() BEFORE closing l.done,
//     i.e. before the worker's own final drainRelayLane runs. If more than
//     one KindSync entry is still queued at that moment, the final drain's
//     TakeSync call merges them and increments Coalesced on l itself (see
//     TakeSync's doc) — AFTER the fold already captured l's stats and AFTER
//     l was removed from s.relayLanes, so that increment lands on a lane
//     object nothing will ever read again either. Unlike gap 1, this
//     requires no adversarial timing: any retirement of a lane whose backlog
//     has more than one pending entry hits it. Fixing it would mean folding
//     AFTER the final drain instead of before, which would require this
//     synchronous fold path to wait on the worker's own goroutine — trading
//     stopRelayLane's/ensureRelayLane's current non-blocking teardown for a
//     wait of unbounded-by-relay-latency duration, which is a worse trade
//     than an occasional missed Coalesced count on an already-degraded lane.
//
// Monotonicity requires holding s.relayLanesMu for THIS METHOD'S ENTIRE
// computation, not just the map snapshot — an earlier, narrower-locking
// draft of this method left a genuine three-call race, identical in shape to
// the one cluster/redis's Relay.Stats() doc documents in full: (1) call A
// takes the lock, reads s.relayRetired (say 0, nothing retired yet) and the
// live map (including lane L for room "r"), then releases the lock before
// summing; (2) stopRelayLane(r, L) runs: takes the write lock, folds L's
// current counters (say Coalesced=50) into s.relayRetired (now 50), deletes L
// from the map, releases the lock; (3) call A, now lockless, calls
// L.lane.Stats() through its own stale reference (the Lane object itself is
// still valid Go memory even though it is no longer reachable from
// s.relayLanes) and happens to read 50 there too (nothing pushed more onto it
// in between) — call A returns retired(0, read in step 1) + 50 (from L) = 50,
// which looks fine in isolation; (4) but a LATER call B takes the lock,
// reads s.relayRetired = 50 (already folded) and finds L no longer in the
// map, so it never adds L's contribution again — call B also returns 50, so
// this particular interleaving is not itself a visible decrease. The version
// that IS visible: if step 3's stale L.lane.Stats() read happens BEFORE
// step 2's fold captures a value that is later increased further by a
// straggling Push (see enqueueRelayOutbound's doc for exactly that
// straggler), call A can return a total that already reflects that
// straggler's contribution while a later call B, seeded only from the
// smaller retired snapshot stopRelayLane folded in, returns less — a real
// decrease across sequential calls. Holding s.relayLanesMu across the WHOLE
// computation below closes this: stopRelayLane (which needs the write lock)
// cannot run between this method's retired-read and its per-lane reads, so
// every value this call sums is one that a later call, seeded from the
// resulting retired total, can only match or exceed. Taking only the RLock
// side (not the full write lock) is sufficient: this method never mutates
// s.relayLanes or s.relayRetired, and RWMutex's readers-block-writer
// semantics are all that is required to keep stopRelayLane (write side) from
// interleaving with this method's read side.
//
// Deadlock/stall trace: this method holds s.relayLanesMu (read) for its
// entire computation, including every live lane's l.lane.Stats() call. That
// used to be a genuine problem, not just a deadlock risk: relaylane.Lane's
// Push and TakeSync both hold the LANE's own mutex across a potentially slow
// crdt.MergeUpdatesV1 call (see collapseLocked's doc), so an earlier version
// of Lane.Stats() that also took that mutex could BLOCK this method's RLock
// hold on room A's in-flight merge — and because Go's sync.RWMutex blocks
// new readers behind a pending writer, a concurrent stopRelayLane/
// ensureRelayLane (write side) queuing behind that stall would then also
// block enqueueRelayOutbound's relayLaneFor RLock for EVERY OTHER room,
// reintroducing exactly the cross-room commit-path coupling #187 exists to
// remove — bounded (it resolves once room A's merge finishes) and not a
// deadlock, but real coupling in the surface built to observe the absence of
// coupling. relaylane.Lane.Stats() is now lock-free (three atomic loads, see
// its doc), so this cannot happen: this method's RLock hold is bounded
// purely by "iterate len(s.relayLanes) lanes, one atomic load each", never by
// anything any lane's Push/TakeSync is doing. With that resolved, the actual
// lock-acquisition graph is: this method and stopRelayLane/ensureRelayLane
// (write side, for their retired-fold) are the only holders of
// s.relayLanesMu, and NOTHING they do while holding it can block — reading
// atomics, deleting/inserting a map entry, and (for the write side)
// close(prev.done)/go relayLaneWorker(...) are all outside the lock already.
// relayLaneFor takes s.relayLanesMu alone. Push (the commit-path hot loop),
// the worker's drain (TakeSync/TakeAwareness), and publishRelay take only a
// Lane's own mutex and never s.relayLanesMu. So there is no lock anywhere
// that nests INSIDE another lock anymore except relayLanesMu wrapping a
// non-blocking atomic read — no cycle, hence no deadlock, and no more
// cross-room stall either.
func (s *Server) RelayStats() RelayStats {
	s.relayLanesMu.RLock()
	defer s.relayLanesMu.RUnlock()

	out := RelayStats{
		Coalesced:           s.relayRetired.Coalesced,
		AwarenessSuperseded: s.relayRetired.AwarenessSuperseded,
		HardDrops:           s.relayRetired.HardDrops,
		Dropped:             s.relayDropped.Load(),
	}
	for _, l := range s.relayLanes {
		st := l.lane.Stats()
		out.Coalesced += st.Coalesced
		out.AwarenessSuperseded += st.AwarenessSuperseded
		out.HardDrops += st.HardDrops
	}
	return out
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
