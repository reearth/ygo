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
func (r *Relay) runRoomWorker(ctx context.Context, w *roomWorker) {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			return
		case <-ctx.Done():
			return
		case <-w.done:
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

// stopWorker removes the room's worker and stops it, draining whatever is
// still queued first so a deactivation does not silently discard delivered-
// but-not-yet-injected updates.
//
// Callers MUST NOT hold r.mu here. The drain below calls Sink.Inject, and if
// a Sink implementation ignores context cancellation (a pre-existing risk —
// see Close's doc comment, which has always had the same exposure via
// wg.Wait joining a worker stuck in the same call) that call can block this
// goroutine indefinitely. That is an acceptable, pre-existing risk for the
// caller of stopWorker to absorb, but it must never escalate into blocking
// r.mu, which would stall every other lifecycle op (Start, Close,
// RoomActivated/RoomDeactivated for unrelated rooms) on the relay.
// RoomDeactivated enforces this by releasing r.mu before calling stopWorker.
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
	if !w.lane.Empty() {
		r.drainLane(r.startCtx, w)
	}
	close(w.done)
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
