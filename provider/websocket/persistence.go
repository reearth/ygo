package websocket

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/reearth/ygo/crdt"
)

// strandedEnter registers a committing goroutine as a potential stranded
// writer. It MUST be called before the observer inspects r.persistRetire: the
// whole point is to be visible to Shutdown from before the decision to write is
// taken, not from after it (#229). Cheap enough to sit on every commit — one
// atomic add.
func (s *Server) strandedEnter() { s.strandedInFlight.Add(1) }

// strandedLeave balances strandedEnter and wakes a waiting Shutdown. The
// non-blocking send is correct even when it drops: a waiter parked on the
// channel leaves the buffer empty, so a later decrement's send lands.
func (s *Server) strandedLeave() {
	s.strandedInFlight.Add(-1)
	if s.strandedWake == nil {
		return
	}
	select {
	case s.strandedWake <- struct{}{}:
	default:
	}
}

// waitStranded blocks until no committing goroutine is in the stranded-write
// path, or ctx expires. Called by Shutdown after the persistence workers have
// been joined — a stranded writer is by construction waiting on a worker that
// has already exited, so this is the only remaining hop between a committed
// transaction and the adapter.
//
// This is a bound, not a guarantee of losslessness. It cannot cover a
// transaction that begins committing AFTER the counter is observed at zero:
// the producers are peer read loops and any holder of a *crdt.Doc, none of
// which the server can join. What it does guarantee is that Shutdown never
// returns while a write it can see is still in flight.
func (s *Server) waitStranded(ctx context.Context) {
	if s.strandedWake == nil {
		return
	}
	for s.strandedInFlight.Load() > 0 {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-s.strandedWake:
		case <-ctx.Done():
			return
		}
	}
}

// persistStranded is the durable destination for updates a room's persistence
// worker can no longer take (#229). It is called from the doc.OnUpdate observer
// registered in loadRoom, on the two paths where the worker has begun retiring:
// an update that could not be enqueued at all (passed as extra), and updates
// that were enqueued but may have missed the worker's final drain.
//
// It first waits for the worker to be completely gone (persistDone). That wait
// is what lets it call the adapter without a lock the worker also holds: the
// adapter contract promises calls for one room are serialised, and after
// persistDone the worker is issuing none. The wait cannot deadlock — the worker
// never waits on a producer, and Shutdown/eviction wait on persistDone too, not
// on this function.
//
// Anything still stranded in persistCh is stored FIRST, then extra, so a single
// stranded call writes in commit order. That is best-effort ordering WITHIN one
// call only: two committing goroutines can be in the observer at once (see
// room.persistFallbackMu), so across concurrent stranded calls the adapter may
// see the room's updates out of commit order. Harmless for a CRDT log — updates
// merge order-independently — but do not read it as a stronger promise than it
// is. Errors are logged, not returned: the observer has no caller to report to.
// Auto-versioning and compaction are deliberately NOT re-run here — the worker
// already performed its final maybeVersion/maybeCompact, and a stranded update
// is by definition a late straggler, not a new steady state.
//
// Callers pay for this synchronously, on the committing goroutine. That is the
// intended trade: it happens only during and after room retirement, and the
// alternative — the pre-#229 behaviour — was silent data loss. Two consequences
// worth stating rather than discovering:
//
//   - crdt.buildPhase2 fires OnUpdate observers in registration order and
//     loadRoom registers persistence BEFORE the relay observers, so a stranded
//     write delays that update's cluster fan-out for as long as the adapter
//     takes. Bounded to room retirement, and a publish onto an already-retired
//     lane is counted in RelayStats().Dropped rather than lost silently — but
//     it does stretch the "no blocking I/O on the commit path" convention
//     further than the rest of the package does.
//   - It writes for a room name the server may already consider gone, and can
//     do so indefinitely if a caller retains the *crdt.Doc. See
//     CompactableAdapter's godoc for what that means for adapters.
func (s *Server) persistStranded(r *room, name string, extra []byte) {
	<-r.persistDone

	r.persistFallbackMu.Lock()
	defer r.persistFallbackMu.Unlock()

	store := func(update []byte) {
		defer func() {
			if rv := recover(); rv != nil {
				log.Printf("ygo/websocket: StoreUpdate panic for retired room %q: %v", name, rv)
			}
		}()
		// context.Background, never the worker's ctx: that ctx is cancelled by
		// the very signal (shutdown / stop) that retired the worker, so using it
		// would make every ctx-aware adapter discard exactly the writes this
		// function exists to save.
		var err error
		if pac, ok := s.persistence.(PersistenceAdapterContext); ok {
			err = pac.StoreUpdateContext(context.Background(), name, update)
		} else {
			err = s.persistence.StoreUpdate(name, update)
		}
		if err != nil {
			log.Printf("ygo/websocket: StoreUpdate for retired room %q: %v", name, err)
		}
	}

	for {
		select {
		case update := <-r.persistCh:
			store(update)
		default:
			if extra != nil {
				store(extra)
			}
			return
		}
	}
}

// startPersistenceWorker spawns the goroutine that drains r.persistCh and
// forwards each update to the PersistenceAdapter. It must be called with
// r.persistCh, r.persistStop, r.persistRetire, and r.persistDone already
// initialised.
//
// The worker exits when either r.persistStop or s.shutdownCh is closed,
// draining any buffered updates before returning so that no committed
// transaction is silently lost.
//
// That guarantee needs a handshake with the producer, not just a final drain
// (#229). A drain is one-shot: producers — peer read loops above all — keep
// committing throughout Shutdown, and Shutdown closes shutdownCh long before it
// closes the peer connections. So the FIRST act of every exit path is to close
// r.persistRetire, publishing "I am leaving" before the final drain rather than
// after it. loadRoom's doc.OnUpdate observer reads that latch and either relies
// on the final drain (its send completed while the latch was open, so the drain
// necessarily follows it) or performs the write itself via persistStranded.
// Without the latch, everything committed after the drain landed in an unread
// 256-slot buffer and vanished.
//
// When coalescing is enabled (see resolveCoalesceConfig / Server.PersistCoalesceWindow),
// bursts of updates are debounced into a single merged write: each new update
// restarts the window timer, and a per-batch maxWait timer bounds staleness.
// When disabled (window < 0), each update is stored individually.
//
// If s.persistence implements PersistenceAdapterContext, store calls are made
// via StoreUpdateContext with a ctx that is cancelled when shutdown or stop
// fires — that cancellation exists so a blocking adapter cannot wedge Shutdown
// indefinitely. Every FINAL flush is the exception, on both paths: it uses
// context.Background() so the last write is not aborted by the very signal that
// triggers it (#229 — the strict path used to pass the cancellable ctx here,
// which made ctx-aware adapters discard the whole tail). Otherwise falls back
// to StoreUpdate (existing behaviour).
//
// When s.persistence also implements CompactableAdapter, Compact is invoked
// on room unload (including server shutdown) and, when s.CompactEvery > 0,
// after every N persistence flushes on the coalescing path.
//
// When s.persistence also implements VersionableAdapter and s.AutoVersionEvery
// > 0, SaveVersion is invoked at most once per AutoVersionEvery per room, and
// only when the room changed since its last version, plus once on room unload if
// it changed after that. The hook lives in store() rather than in either select
// loop, so both the coalescing and the strict per-update paths get it.
func (s *Server) startPersistenceWorker(r *room, name string) {
	clock := s.clock
	if clock == nil {
		clock = realClock{}
	}
	mergeFn := s.mergeFn
	if mergeFn == nil {
		mergeFn = crdt.MergeUpdatesV1
	}
	enabled, window, maxWait := resolveCoalesceConfig(s.PersistCoalesceWindow, s.PersistCoalesceMaxWait)

	// Auto-versioning setup, resolved BEFORE the goroutine starts. versioner is
	// left nil when the feature is off or unsupported, which makes maybeVersion a
	// cheap no-op on the hot path.
	versioner, _ := s.persistence.(VersionableAdapter)
	autoVersionEvery := s.AutoVersionEvery
	if autoVersionEvery <= 0 {
		versioner = nil
	}
	// lastVersion is stamped here, not inside the goroutine, so the interval is
	// measured from when the worker was CREATED rather than from whenever its
	// goroutine happened to be scheduled.
	lastVersion := clock.now()

	go func() {
		defer close(r.persistDone)

		// retire publishes "this worker is leaving" to the doc.OnUpdate producer
		// (#229). Ordering is load-bearing in two directions:
		//
		//   - it must run BEFORE the final drain, so a send that completed while
		//     the latch was open is guaranteed to be picked up by that drain;
		//   - it must run BEFORE close(r.persistDone), so a producer that sees
		//     the latch and blocks on persistDone is released only once the
		//     worker really is finished with the adapter.
		//
		// The defer is the backstop for an exit no explicit call covers (a panic
		// escaping the loop): deferred calls run LIFO, so this fires before the
		// close(r.persistDone) registered above it. sync.Once keeps the explicit
		// exit-path calls and this backstop from double-closing.
		var retireOnce sync.Once
		retire := func() { retireOnce.Do(func() { close(r.persistRetire) }) }
		defer retire()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Cancel the adapter ctx when shutdown or stop fires.
		go func() {
			select {
			case <-s.shutdownCh:
			case <-r.persistStop:
			}
			cancel()
		}()

		compactor, _ := s.persistence.(CompactableAdapter)
		compactEvery := s.CompactEvery
		flushCount := 0

		// dirtySinceVersion, like lastVersion above, is mutated only from
		// store()/maybeVersion on THIS goroutine, so it needs no synchronisation.
		dirtySinceVersion := false

		// maybeVersion asks the adapter for a labelled version, but only when the
		// room actually changed since the last one (dirtySinceVersion) and either
		// the interval has elapsed or force is set (room unload). That pairing is
		// the anti-churn guarantee: a quiet room is never versioned, and an active
		// room yields at most one version per AutoVersionEvery.
		//
		// Contained by recover; errors and panics are logged and never fatal.
		// On failure lastVersion is still advanced so a permanently failing adapter
		// is retried once per interval rather than on every flush, while
		// dirtySinceVersion stays set so the change is not forgotten.
		maybeVersion := func(force bool) {
			if versioner == nil || !dirtySinceVersion {
				return
			}
			if !force && clock.now().Sub(lastVersion) < autoVersionEvery {
				return
			}
			defer func() {
				if rv := recover(); rv != nil {
					lastVersion = clock.now()
					log.Printf("ygo/websocket: SaveVersion panic for room %q: %v", name, rv)
				}
			}()
			_, err := versioner.SaveVersion(context.Background(), name, AutoVersionLabel)
			lastVersion = clock.now()
			if err != nil {
				log.Printf("ygo/websocket: SaveVersion for room %q: %v", name, err)
				return
			}
			dirtySinceVersion = false
		}

		// maybeCompact calls the adapter's Compact (if any) with a background
		// ctx, contained by recover; errors/panics are logged, never fatal.
		maybeCompact := func() {
			if compactor == nil {
				return
			}
			defer func() {
				if rv := recover(); rv != nil {
					log.Printf("ygo/websocket: Compact panic for room %q: %v", name, rv)
				}
			}()
			if err := compactor.Compact(context.Background(), name); err != nil {
				log.Printf("ygo/websocket: Compact for room %q: %v", name, err)
			}
		}

		// onFlushed records a non-empty flush and triggers count-based compaction.
		onFlushed := func(wrote bool) {
			if !wrote || compactEvery <= 0 || compactor == nil {
				return
			}
			flushCount++
			if flushCount >= compactEvery {
				flushCount = 0
				maybeCompact()
			}
		}

		// store issues one backing-store write for a single update using the
		// given ctx and returns whether it failed. Panics are contained and
		// reported as a non-nil error; errors are logged (no retry).
		store := func(ctx context.Context, update []byte) (err error) {
			defer func() {
				if rv := recover(); rv != nil {
					log.Printf("ygo/websocket: StoreUpdate panic for room %q: %v", name, rv)
					err = fmt.Errorf("StoreUpdate panic for room %q: %v", name, rv)
				}
			}()
			if pac, ok := s.persistence.(PersistenceAdapterContext); ok {
				err = pac.StoreUpdateContext(ctx, name, update)
			} else {
				err = s.persistence.StoreUpdate(name, update)
			}
			if err != nil {
				log.Printf("ygo/websocket: StoreUpdate for room %q: %v", name, err)
				return err
			}
			// A durable write is the one signal that the room changed, and it is
			// common to BOTH the coalescing and the strict per-update paths, so
			// auto-versioning hooks in here rather than in either select loop.
			dirtySinceVersion = true
			maybeVersion(false)
			return nil
		}

		// flush merges batch into one update and stores it. On merge failure it
		// stores each update individually so a bad merge never drops data. It
		// returns true only if the batch is fully persisted and therefore safe to
		// discard; a false result (e.g. ctx cancelled at shutdown, or a transient
		// store error) means the caller must RETAIN the batch so it can be
		// re-flushed — otherwise a batch could be lost if a timer flush races with
		// shutdown cancelling the worker ctx.
		flush := func(ctx context.Context, batch [][]byte) bool {
			if len(batch) == 0 {
				return true
			}
			if len(batch) == 1 {
				return store(ctx, batch[0]) == nil
			}
			merged, err := mergeFn(batch...)
			if err != nil {
				log.Printf("ygo/websocket: merge of %d updates for room %q failed (%v); storing individually", len(batch), name, err)
				ok := true
				for _, u := range batch {
					if store(ctx, u) != nil {
						ok = false
					}
				}
				return ok
			}
			return store(ctx, merged) == nil
		}

		// Strict per-update path (coalescing disabled): one store per update.
		if !enabled {
			// pending holds updates whose store was ABORTED by ctx cancellation
			// rather than rejected by the adapter. Cancellation is a shutdown/stop
			// signal, not a verdict on the data, so the update is retained and
			// re-stored on the exit path under a background ctx — the same
			// retain-and-re-flush discipline the coalescing path already applies to
			// an unflushed batch. Without it a ctx-aware adapter loses every update
			// the worker happens to pick up in the window between shutdown
			// cancelling the ctx and the worker reaching its exit case (#229).
			// Mutated only from this goroutine.
			var pending [][]byte

			// storeRetaining is store() plus that retention rule.
			storeRetaining := func(c context.Context, update []byte) bool {
				if err := store(c, update); err != nil {
					if c.Err() != nil {
						pending = append(pending, update)
					}
					return false
				}
				return true
			}

			// drain stores everything currently buffered and reports whether every
			// store succeeded, so an on-demand flush can act as a durability barrier.
			drain := func(c context.Context) bool {
				ok := true
				for {
					select {
					case update := <-r.persistCh:
						if !storeRetaining(c, update) {
							ok = false
						}
					default:
						return ok
					}
				}
			}
			// exit runs the shared retire-then-drain sequence for both exit
			// triggers. retire() FIRST (see the retire comment above), then the
			// retained updates and the final drain under a BACKGROUND ctx so a
			// ctx-aware adapter is not handed the context that shutdown itself
			// just cancelled (#229 — this path used to pass the cancellable one).
			exit := func() {
				retire()
				retained := pending
				pending = nil
				for _, u := range retained {
					_ = store(context.Background(), u)
				}
				drain(context.Background())
				maybeVersion(true)
				maybeCompact()
			}
			for {
				select {
				case update := <-r.persistCh:
					_ = storeRetaining(ctx, update)
				case ack := <-r.flushReq:
					ok := drain(ctx) // store everything currently buffered
					ack <- ok
				case <-r.persistStop:
					exit()
					return
				case <-s.shutdownCh:
					exit()
					return
				}
			}
		}

		// Coalescing path.
		var batch [][]byte
		var windowT, maxT wsTimer

		clearTimers := func() {
			if windowT != nil {
				windowT.stop()
				windowT = nil
			}
			if maxT != nil {
				maxT.stop()
				maxT = nil
			}
		}
		// drainBuffered pulls everything currently queued into batch without
		// blocking. Used on stop/shutdown before the final flush.
		drainBuffered := func() {
			for {
				select {
				case update := <-r.persistCh:
					batch = append(batch, update)
				default:
					return
				}
			}
		}
		// exit runs the shared retire-then-drain-then-flush sequence for both
		// exit triggers. retire() must come FIRST so the doc.OnUpdate producer
		// can never hand an update to a channel this worker has already swept
		// for the last time (#229); the flush uses a background ctx so the last
		// batched write survives the very signal that triggered the exit.
		exit := func() {
			retire()
			drainBuffered()
			flush(context.Background(), batch)
			clearTimers() // release any pending timers before exit
			maybeVersion(true)
			maybeCompact()
		}

		for {
			var windowC, maxC <-chan time.Time
			if windowT != nil {
				windowC = windowT.ch()
			}
			if maxT != nil {
				maxC = maxT.ch()
			}
			select {
			case update := <-r.persistCh:
				batch = append(batch, update)
				if windowT == nil {
					// First update of a new batch: arm both timers.
					windowT = clock.newTimer(window)
					maxT = clock.newTimer(maxWait)
				} else {
					// Debounce: restart the window timer; leave maxWait (it is
					// measured from the batch's first update). Create-new avoids
					// any Reset/drain hazard.
					windowT.stop()
					windowT = clock.newTimer(window)
				}
			case <-windowC:
				// Retain the batch if the flush did not fully persist: a timer
				// flush can race with shutdown cancelling ctx, and niling an
				// unpersisted batch would lose it. Timers are cleared regardless;
				// a retained batch re-flushes on the next update or at exit.
				// A batch retained here has no self-driven retry timer in steady
				// state; it is re-flushed at the stop/shutdown exit case below
				// with a background context.
				wrote := len(batch) > 0
				ok := flush(ctx, batch)
				if ok {
					batch = nil
				}
				clearTimers()
				onFlushed(wrote && ok)
			case <-maxC:
				wrote := len(batch) > 0
				ok := flush(ctx, batch)
				if ok {
					batch = nil
				}
				clearTimers()
				onFlushed(wrote && ok)
			case ack := <-r.flushReq:
				// The just-arrived edit may still be in persistCh, not yet in
				// batch — drain first so an on-demand flush never misses it.
				drainBuffered()
				wrote := len(batch) > 0
				ok := flush(context.Background(), batch)
				if ok {
					batch = nil
				}
				clearTimers()
				onFlushed(wrote && ok)
				// Report success so teardown can gate eviction on real durability.
				// On failure the batch is retained (not niled) for a later retry.
				// ack is buffered (cap 1) by every caller, so this never blocks.
				ack <- ok
			case <-r.persistStop:
				exit()
				return
			case <-s.shutdownCh:
				exit()
				return
			}
		}
	}()
}
