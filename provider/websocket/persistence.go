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
// fires — EXCEPT the final stop/shutdown flush, which uses context.Background()
// so the last batched write is not aborted by the very signal that triggers it.
// Otherwise falls back to StoreUpdate (existing behaviour).
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
			}
			return err
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
			drain := func() {
				for {
					select {
					case update := <-r.persistCh:
						store(ctx, update)
					default:
						return
					}
				}
			}
			for {
				select {
				case update := <-r.persistCh:
					store(ctx, update)
				case <-r.persistStop:
					drain()
					return
				case <-s.shutdownCh:
					drain()
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
				if flush(ctx, batch) {
					batch = nil
				}
				clearTimers()
			case <-maxC:
				if flush(ctx, batch) {
					batch = nil
				}
				clearTimers()
			case <-r.persistStop:
				drainBuffered()
				flush(context.Background(), batch) // non-cancelled: survive shutdown
				return
			case <-s.shutdownCh:
				drainBuffered()
				flush(context.Background(), batch)
				return
			}
		}
	}()
}
