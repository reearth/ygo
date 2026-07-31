// Inbound delivery workers: one goroutine per room draining that room's lane
// into Sink.Inject. See redis.go for the transport and lifecycle.

package redis

import (
	"context"
	"errors"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/internal/relaylane"
)

// roomWorker owns inbound delivery for exactly one room. The router pushes
// onto lane without blocking; the worker goroutine drains it and calls
// Sink.Inject, so a slow Inject stalls only its own room (#187).
type roomWorker struct {
	room string
	lane *relaylane.Lane
	done chan struct{} // closed to stop this worker
}

// workerFor returns the worker for room, creating and starting it if needed.
// Safe to call from the router hot path and from RoomActivated.
func (r *Relay) workerFor(room string) *roomWorker {
	r.workersMu.Lock()
	defer r.workersMu.Unlock()
	if w, ok := r.workers[room]; ok {
		return w
	}
	w := &roomWorker{
		room: room,
		lane: relaylane.New(r.laneCap),
		done: make(chan struct{}),
	}
	r.workers[room] = w
	r.wg.Add(1)
	// Reading r.startCtx here without a lock is safe for the same reason
	// Relay.Publish's startCtx read is safe (see its "Safe to read r.startCtx
	// unlocked" comment in redis.go): started.Store(true) is a release
	// barrier in Start, so any goroutine that got here — either via the
	// router (which only runs after Start launched it) or via RoomActivated
	// (which additionally holds r.mu) — observes the startCtx write that
	// preceded it. Do not add a lock around startCtx here: this is the
	// router hot path and must not contend with lifecycle ops.
	go r.runRoomWorker(r.startCtx, w)
	return w
}

// runRoomWorker drains one room's lane until the relay closes, the bound
// context is cancelled, or the worker is stopped.
//
// The w.done case performs one final drainLane before returning, rather than
// returning immediately. This worker is the lane's ONLY consumer for its
// entire life — stopWorker (which closes w.done) deliberately does not touch
// the lane itself, precisely so nothing else ever drains concurrently with
// this goroutine. And because this goroutine is registered on r.wg (see
// workerFor), that final drain is joined by Close's wg.Wait(): there is no
// separate, unjoined goroutine that could still be mid-Inject after Close
// returns.
func (r *Relay) runRoomWorker(ctx context.Context, w *roomWorker) {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			return
		case <-ctx.Done():
			return
		case <-w.done:
			r.drainLane(ctx, w)
			return
		case <-w.lane.Signal():
			r.drainLane(ctx, w)
		}
	}
}

// drainLane delivers everything currently pending on w's lane. Signal is a
// coalescing notification, so this must loop until both takes report empty
// rather than assuming one signal means one payload.
//
// The two kinds are drained in strict alternation (sync, awareness, sync,
// awareness, ...) rather than always taking sync first: an always-sync-first
// order lets sustained sync traffic starve awareness indefinitely, because a
// fresh sync payload can be queued again by the time the previous Inject
// returns. Awareness is self-healing heartbeat state so unbounded starvation
// was only ever a latency issue, not a correctness one, but alternation
// bounds it outright at no extra cost.
func (r *Relay) drainLane(ctx context.Context, w *roomWorker) {
	preferAwareness := false
	for {
		// Preserve the Close invariant (redis.go's Close doc): a payload
		// buffered when Close fires must not reach the Sink afterwards.
		if r.closed.Load() {
			return
		}
		if preferAwareness {
			if data, ok := w.lane.TakeAwareness(); ok {
				preferAwareness = false
				r.inject(ctx, w.room, cluster.KindAwareness, data)
				continue
			}
			if data, ok := w.lane.TakeSync(); ok {
				r.inject(ctx, w.room, cluster.KindSync, data)
				continue
			}
			return
		}
		if data, ok := w.lane.TakeSync(); ok {
			preferAwareness = true
			r.inject(ctx, w.room, cluster.KindSync, data)
			continue
		}
		if data, ok := w.lane.TakeAwareness(); ok {
			r.inject(ctx, w.room, cluster.KindAwareness, data)
			continue
		}
		return
	}
}

// stopWorker removes the room's worker from the map and signals it to stop.
// It deliberately does NOT drain the lane itself — see runRoomWorker's
// w.done case, which performs the final drain on the worker's own goroutine
// instead. That goroutine is the lane's sole consumer for its whole life, so
// nothing here can race it for the same queued payloads, and because that
// goroutine is on r.wg, its final drain is joined by Close.
//
// This makes stopWorker itself fast and non-blocking (a map delete and a
// channel close, nothing that can wait on Sink.Inject), so — unlike an
// earlier version of this function — callers do not need to avoid holding
// r.mu across it.
func (r *Relay) stopWorker(room string) {
	r.workersMu.Lock()
	w, ok := r.workers[room]
	if ok {
		delete(r.workers, room)
	}
	r.workersMu.Unlock()
	if !ok {
		return
	}
	close(w.done)
}

// isRoomActive reports whether room currently has a positive activation
// refcount on this relay (RoomActivated/RoomDeactivated) — a purely
// relay-level notion, independent of whether the Sink knows about the room
// at all (Sink.Inject auto-creates non-resident rooms; that is a separate
// contract exercised by TestInteg_Subscriber_NonResidentRoom_StillInjects,
// which itself RoomActivates the room, so it stays "active" here too).
//
// Takes r.mu, so it must never be called by a goroutine that already holds
// r.mu. RoomActivated does not need it: by the time it would want to know
// this, it has just incremented the counter itself.
func (r *Relay) isRoomActive(room string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeRooms[room] > 0
}

// workerForInbound is the router's (runSubscriber's) entry point for
// resolving a room's worker, as opposed to workerFor, which RoomActivated
// uses directly. The hit path (existing worker in r.workers) is identical to
// workerFor and, like it, touches only workersMu.
//
// The miss path differs: workerFor always creates unconditionally, which is
// correct for RoomActivated (the room is, by construction, active) but wrong
// for the router. A miss here is reachable in two ways: an explicit reap
// while the room stays active (see
// TestInteg_StopWorker_LazyRecreateOnStillSubscribedRoom, where recreating is
// exactly the wanted behaviour), or a straggler message already buffered in
// go-redis's Channel — or in flight across the UNSUBSCRIBE — for a room this
// relay just deactivated. RoomDeactivated cannot reap a worker created by
// the latter case: activeRooms is already back at zero, so a later
// RoomDeactivated call for the same room just no-ops, and the stray would
// otherwise live until Close. That is exactly the unbounded per-room growth
// this task exists to stop, so a miss only creates a worker if the room is
// still active; a message for an inactive room is dropped, symmetric with
// the self-delivery drop (H2) in runSubscriber — expected, not a bug.
//
// isRoomActive takes r.mu, but only on this (rare) miss path — the hit path
// above never touches r.mu, so steady-state message routing does not
// contend with lifecycle ops. There is a narrow residual race between the
// isRoomActive check and workerFor's creation (RoomDeactivated could
// complete in between): that would still create one stray worker, no
// different from any other, which lives until Close if the room is never
// reactivated. This does not reintroduce the leak — it narrows an
// unconditional, routine occurrence down to a rare TOCTOU window between two
// independent lock acquisitions.
func (r *Relay) workerForInbound(room string) (w *roomWorker, ok bool) {
	r.workersMu.Lock()
	w, ok = r.workers[room]
	r.workersMu.Unlock()
	if ok {
		return w, true
	}
	if !r.isRoomActive(room) {
		return nil, false
	}
	return r.workerFor(room), true
}

// inject hands one payload to the Sink, logging failures at Warn (a transient
// Inject error must not kill the room's worker). A context.Canceled error
// (e.g. from Server.Shutdown cancelling the relay's bound context) is
// expected during shutdown and is not logged, mirroring the behaviour the
// old inline Inject call in runSubscriber had.
func (r *Relay) inject(ctx context.Context, room string, kind cluster.Kind, data []byte) {
	err := r.sink.Inject(ctx, cluster.Inbound{Room: room, Kind: kind, Data: data})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		r.log.Warn("cluster/redis: sink.Inject failed",
			"room", room, "kind", kind, "err", err)
	}
}
