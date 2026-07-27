package websocket

import (
	"context"
	"sort"
	"time"
)

// defaultIdleSweepCap bounds the derived sweep cadence so a very large
// RoomIdleTimeout does not leave eviction latency unbounded.
const defaultIdleSweepCap = 30 * time.Second

// ensureIdleSweeper starts the single per-Server idle-room sweeper goroutine the
// first time it is called while RoomIdleTimeout > 0. It is a cheap sync.Once
// fast-path on every subsequent call and a complete no-op in eager-evict mode
// (RoomIdleTimeout == 0), so calling it on every room creation is safe. The
// goroutine runs until Shutdown closes s.shutdownCh, then closes s.sweeperDone.
func (s *Server) ensureIdleSweeper() {
	if s.RoomIdleTimeout <= 0 {
		return
	}
	s.sweeperOnce.Do(func() {
		s.sweeperStarted.Store(true)
		go s.idleSweepLoop()
	})
}

// idleSweepInterval returns how often the sweeper wakes. Tests may set
// s.idleSweepInterval for a short deterministic period; otherwise it is derived
// from RoomIdleTimeout (half the timeout, clamped to [1s, defaultIdleSweepCap])
// so eviction latency stays bounded without polling needlessly often.
func (s *Server) idleSweepIntervalDur() time.Duration {
	if s.idleSweepInterval > 0 {
		return s.idleSweepInterval
	}
	d := s.RoomIdleTimeout / 2
	if d < time.Second {
		d = time.Second
	}
	if d > defaultIdleSweepCap {
		d = defaultIdleSweepCap
	}
	return d
}

// idleSweepLoop is the sweeper goroutine body. It ticks at idleSweepIntervalDur
// and evicts expired / over-cap idle rooms each tick until Shutdown fires. It
// closes s.sweeperDone on exit so Shutdown / tests can observe a clean stop.
func (s *Server) idleSweepLoop() {
	defer close(s.sweeperDone)
	ticker := time.NewTicker(s.idleSweepIntervalDur())
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			s.sweepIdleRooms(time.Now())
		}
	}
}

// idleCandidate is a snapshot of one idle-resident room taken under s.rmu.
type idleCandidate struct {
	name      string
	rm        *room
	idleSince time.Time
}

// sweepIdleRooms performs one sweep pass. It snapshots the idle-resident rooms
// under s.rmu (O(1) per room; no I/O and no flush under the lock), then decides
// which to evict: any room idle longer than RoomIdleTimeout, plus — when
// MaxResidentRooms > 0 — the least-recently-idle rooms in excess of the bound
// (LRU). Each eviction goes through evictIdleRoom, which re-checks emptiness
// under rm.mu and performs the v1.37.0 flush-before-evict teardown. s.rmu is
// never held across a flush or eviction.
func (s *Server) sweepIdleRooms(now time.Time) {
	timeout := s.RoomIdleTimeout
	if timeout <= 0 {
		return
	}

	// Snapshot idle-resident rooms. A room counts as idle only when it is empty
	// (len(peers)==0) AND carries a non-zero idleSince stamp — i.e. genuinely
	// idle, not merely looked up. Skip rooms still loading (ready not closed):
	// idleSince is only ever stamped post-load, so this is belt-and-braces.
	var idle []idleCandidate
	s.rmu.RLock()
	for name, rm := range s.rooms {
		select {
		case <-rm.ready:
		default:
			continue
		}
		rm.mu.Lock()
		if len(rm.peers) == 0 && !rm.idleSince.IsZero() {
			idle = append(idle, idleCandidate{name: name, rm: rm, idleSince: rm.idleSince})
		}
		rm.mu.Unlock()
	}
	s.rmu.RUnlock()

	if len(idle) == 0 {
		return
	}

	// Order least-recently-idle first so the LRU overflow slice is the oldest
	// rooms and eviction below happens oldest-first.
	sort.Slice(idle, func(i, j int) bool {
		return idle[i].idleSince.Before(idle[j].idleSince)
	})

	// overflow is how many of the oldest idle rooms must go purely to satisfy the
	// MaxResidentRooms LRU bound, independent of the timeout.
	overflow := 0
	if s.MaxResidentRooms > 0 && len(idle) > s.MaxResidentRooms {
		overflow = len(idle) - s.MaxResidentRooms
	}

	for i, c := range idle {
		expired := now.Sub(c.idleSince) >= timeout
		lru := i < overflow
		if expired || lru {
			// evictIdleRoom re-verifies peers==0 and the exact idle stamp under
			// rm.mu before committing, so a room that was rejoined or re-stamped
			// between the snapshot and here is safely left alone.
			s.evictIdleRoom(c.name, c.rm, c.idleSince)
		}
	}
}

// evictIdleRoom evicts a single idle-resident room using the identical teardown
// primitives as handleDisconnect / CloseRoom: a flush-before-evict durability
// barrier (flushReq/ack, no lock held across it), a pointer-identity guarded
// delete from s.rooms under s.rmu+rm.mu, closing persistStop, waiting persistDone,
// stopping awareness auto-expiry, tearing down relay observers, and firing
// OnUnloadDocument. It is purpose-built for the sweeper rather than shared with
// those two paths (whose eviction is interleaved with peer removal, semaphore
// release, OnLastPeer, and awareness-removal broadcasts) so their reviewed code
// is left untouched; the teardown sequence itself is byte-for-byte the same.
//
// CRITICAL: expectIdle is the idleSince observed in the snapshot. Eviction
// commits only if, under rm.mu, the room is STILL empty AND idleSince is
// unchanged. A rejoin clears idleSince (→ zero); any join/leave churn restamps
// it to a different time.Now(); relay/Apply activity clears it via clearIdle.
// All three make the Equal check fail, so the sweeper never evicts a room that
// became active — it evicts on emptiness, never on the stale stamp alone.
//
// On flush failure the room + worker are kept alive (matching handleDisconnect):
// the retained batch is retried on the next sweep / teardown, and a reconnect
// meanwhile finds the warm in-memory doc rather than reloading a stale store.
func (s *Server) evictIdleRoom(name string, rm *room, expectIdle time.Time) bool {
	// FLUSH-BEFORE-EVICT (#175 follow-up). Make the pending coalesced batch
	// durable while rm is still in s.rooms and the worker is still alive. No lock
	// is held across the send/ack; the ack is buffered (cap 1) so the worker
	// never blocks. Guard against a worker that already exited (raced by
	// CloseRoom/handleDisconnect/shutdown) via the persistDone branch.
	flushOK := true
	if rm.flushReq != nil {
		ack := make(chan bool, 1)
		select {
		case rm.flushReq <- ack:
			flushOK = <-ack
		case <-rm.persistDone:
			// Worker already gone; its exit-flush handled durability best-effort.
		}
	}
	if !flushOK {
		// Abort: keep room + worker alive so the retained batch is retried later.
		return false
	}

	evicted := false
	s.rmu.Lock()
	rm.mu.Lock()
	// Re-check under the locks: only evict a room that is STILL empty and STILL
	// carries the exact idle stamp we snapshotted. This mirrors handleDisconnect's
	// own re-check and is the guarantee that we never evict on idleSince alone.
	if len(rm.peers) == 0 && rm.idleSince.Equal(expectIdle) {
		if current, ok := s.rooms[name]; ok && current == rm {
			delete(s.rooms, name)
			evicted = true
		}
		if rm.persistStop != nil {
			select {
			case <-rm.persistStop:
				// already closed by a racing CloseRoom
			default:
				close(rm.persistStop)
			}
		}
	}
	rm.mu.Unlock()
	s.rmu.Unlock()

	if !evicted {
		return false
	}

	// Wait for the persistence goroutine to drain and exit before the room
	// reference becomes garbage.
	if rm.persistDone != nil {
		<-rm.persistDone
	}
	// Stop awareness auto-expiry, tear down relay observers, fire OnUnloadDocument.
	rm.awareness.Destroy()
	s.teardownRelayRoom(rm, name)
	if hook := s.OnUnloadDocument; hook != nil {
		s.safeHook("OnUnloadDocument", func() {
			hook(context.Background(), name)
		})
	}
	return true
}
