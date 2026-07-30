package websocket

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/reearth/ygo/crdt"
)

// startPersistenceWorker spawns the goroutine that drains r.persistCh and
// forwards each update to the PersistenceAdapter. It must be called with
// r.persistCh, r.persistStop, and r.persistDone already initialised.
//
// The worker exits when either r.persistStop or s.shutdownCh is closed,
// draining any buffered updates before returning so that no committed
// transaction is silently lost.
//
// When coalescing is enabled (see resolveCoalesceConfig / Server.PersistCoalesceWindow),
// bursts of updates are debounced into a single merged write: each new update
// restarts the window timer, and a per-batch maxWait timer bounds staleness.
// When disabled (window < 0), each update is stored individually.
//
// If s.persistence implements PersistenceAdapterContext, store calls are made
// via StoreUpdateContext with a ctx that is cancelled when shutdown or stop
// fires. On the coalescing path the final stop/shutdown flush is the exception:
// it uses context.Background() so the last batched write is not aborted by the
// very signal that triggers it. On the disabled (per-update) path the final
// drain uses the cancellable ctx — matching pre-v1.36 behaviour — so a
// context-aware adapter may still see cancellation during that drain.
// Otherwise falls back to StoreUpdate (existing behaviour).
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

		// Strict per-update path (coalescing disabled): unchanged behaviour.
		if !enabled {
			// drain stores everything currently buffered and reports whether every
			// store succeeded, so an on-demand flush can act as a durability barrier.
			drain := func() bool {
				ok := true
				for {
					select {
					case update := <-r.persistCh:
						if store(ctx, update) != nil {
							ok = false
						}
					default:
						return ok
					}
				}
			}
			for {
				select {
				case update := <-r.persistCh:
					_ = store(ctx, update)
				case ack := <-r.flushReq:
					ok := drain() // store everything currently buffered
					ack <- ok
				case <-r.persistStop:
					drain()
					maybeVersion(true)
					maybeCompact()
					return
				case <-s.shutdownCh:
					drain()
					maybeVersion(true)
					maybeCompact()
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
				drainBuffered()
				flush(context.Background(), batch) // non-cancelled: survive shutdown
				clearTimers()                      // release any pending timers before exit
				maybeVersion(true)
				maybeCompact()
				return
			case <-s.shutdownCh:
				drainBuffered()
				flush(context.Background(), batch)
				clearTimers() // release any pending timers before exit
				maybeVersion(true)
				maybeCompact()
				return
			}
		}
	}()
}
