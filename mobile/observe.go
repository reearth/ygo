package mobile

import (
	"bytes"
	"encoding/json"
	"sort"
	"sync"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
)

// DocObserver receives a notification after each committed transaction.
// updateV1 is the incremental V1 update; local is true when the change
// originated from a mobile mutator on this Doc (vs a remote ApplyUpdate), so an
// app that also syncs to a server can avoid echo loops. OnChange runs on a
// background goroutine — never the UI thread, never under a lock.
type DocObserver interface {
	OnChange(updateV1 []byte, local bool)
}

// Subscription detaches a bound observer. Close is idempotent and safe from any
// goroutine.
type Subscription struct {
	once        sync.Once
	unsubscribe func()
	stopFn      func() // wakes + stops the drain goroutine; nil for a closed stub
	detach      func() // remove from owner registry; nil for a stub
}

// Close detaches the observer and stops delivery. It is idempotent and safe to
// call from any goroutine. Close signals the drain goroutine to stop but does
// NOT join it: the drain goroutine may be inside OnChange (which can re-enter a
// mobile method needing the Doc lock), so joining while a lock is held would
// deadlock. After Close, no further OnChange calls occur.
func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.detach != nil {
			s.detach()
		}
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		if s.stopFn != nil {
			s.stopFn()
		}
	})
}

// closedSubscription returns a non-nil Subscription whose Close is a no-op, for
// Observe-after-Close.
func closedSubscription() *Subscription {
	s := &Subscription{}
	s.once.Do(func() {})
	return s
}

// emptyUpdateV1 is the canonical V1 update for a transaction that changed
// nothing. Precomputed so the Doc bridge can drop no-op notifications.
var emptyUpdateV1 = func() []byte {
	d := crdt.New()
	var captured []byte
	unsub := d.OnUpdate(func(u []byte, _ any) { captured = u })
	d.Transact(func(*crdt.Transaction) {}) // no-op still fires OnUpdate when a sub is present
	unsub()
	return captured
}()

func isEmptyUpdateV1(u []byte) bool { return len(u) == 0 || bytes.Equal(u, emptyUpdateV1) }

// docPending is the coalescing mailbox for one Doc subscription. Under the
// normal path the queue stays length ≤1 (each new update merges into the tail);
// it only grows on the unreachable MergeUpdatesV1-error path, where a new update
// is appended rather than dropped — bounded and lossless either way.
type docPending struct {
	mu      sync.Mutex
	cond    *sync.Cond
	updates [][]byte // FIFO; coalesced into the tail when possible; nil = empty
	locals  []bool   // parallel local flag per queued update
	stopped bool
}

// Observe registers obs to be notified after each committed transaction on this
// Doc. Notifications are delivered on a dedicated background goroutine, in
// commit order, with no locks held. The returned Subscription detaches the
// observer when Closed. If the Doc is already Closed, a non-nil
// already-closed Subscription is returned (its Close is a no-op).
func (m *Doc) Observe(obs DocObserver) *Subscription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return closedSubscription()
	}
	p := &docPending{}
	p.cond = sync.NewCond(&p.mu)

	// Bridge callback: runs under the committing goroutine's lock. It must be
	// cheap — it only drops empty updates, computes local, and enqueues under
	// p.mu. It never takes m.mu, never calls the observer, never blocks.
	unsub := m.d.OnUpdate(func(update []byte, origin any) {
		if isEmptyUpdateV1(update) {
			return
		}
		local := origin == mobileLocalOrigin
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return
		}
		if n := len(p.updates); n == 0 {
			p.updates = append(p.updates, update)
			p.locals = append(p.locals, local)
		} else if merged, err := crdt.MergeUpdatesV1(p.updates[n-1], update); err == nil {
			// Normal path: coalesce into the tail, keeping the queue length ≤1.
			p.updates[n-1] = merged
			p.locals[n-1] = p.locals[n-1] && local
		} else {
			// Unreachable in practice (merging two valid V1 updates never
			// errors). Append rather than drop so no op is ever lost.
			p.updates = append(p.updates, update)
			p.locals = append(p.locals, local)
		}
		p.cond.Signal()
		p.mu.Unlock()
	})

	sub := &Subscription{
		unsubscribe: unsub,
		stopFn: func() {
			p.mu.Lock()
			p.stopped = true
			p.cond.Broadcast()
			p.mu.Unlock()
		},
	}
	sub.detach = func() { m.detachSub(sub) }

	go docDrain(p, obs)

	m.subsMu.Lock()
	if m.subs == nil {
		m.subs = make(map[*Subscription]struct{})
	}
	m.subs[sub] = struct{}{}
	m.subsMu.Unlock()
	return sub
}

// docDrain delivers coalesced updates to obs, one at a time, in commit order,
// with no locks held. It exits when the subscription is stopped, abandoning any
// still-queued update (no callback after Close).
func docDrain(p *docPending, obs DocObserver) {
	for {
		p.mu.Lock()
		for len(p.updates) == 0 && !p.stopped {
			p.cond.Wait()
		}
		if p.stopped { // abandon the whole queue; no callback after Close
			p.mu.Unlock()
			return
		}
		upd := p.updates[0]
		local := p.locals[0]
		// Pop the front; nil the vacated slot so the backing array can release
		// the delivered update.
		p.updates[0] = nil
		p.updates = p.updates[1:]
		p.locals = p.locals[1:]
		p.mu.Unlock()
		obs.OnChange(upd, local) // off all locks
	}
}

func (m *Doc) detachSub(s *Subscription) {
	m.subsMu.Lock()
	delete(m.subs, s)
	m.subsMu.Unlock()
}

// AwarenessObserver receives the changed client-id sets after each presence
// change. changesJSON is `{"added":[..],"updated":[..],"removed":[..]}` with the
// three id arrays sorted ascending and always present (`[]` never `null`). The
// sets are advisory: the app reads StatesJSON for the authoritative presence
// snapshot, so an over-notified id is harmless. OnChange runs on a background
// goroutine — never the UI thread, never under a lock.
type AwarenessObserver interface {
	OnChange(changesJSON []byte)
}

// awarenessChanges is the JSON payload delivered to an AwarenessObserver. The
// slices are always non-nil so each field marshals to `[]` (never `null`).
type awarenessChanges struct {
	Added   []uint64 `json:"added"`
	Updated []uint64 `json:"updated"`
	Removed []uint64 `json:"removed"`
}

// awarenessPending coalesces presence changes under backpressure by UNION-ing
// the id sets. The sets are advisory (the app reads StatesJSON for truth), so
// over-notifying an id is safe and needs no net-effect reconciliation — unlike
// the Doc bridge there is no per-op payload to merge losslessly, only ids to
// accumulate. The queue therefore never grows: it is a single coalesced batch.
type awarenessPending struct {
	mu      sync.Mutex
	cond    *sync.Cond
	added   map[uint64]struct{}
	updated map[uint64]struct{}
	removed map[uint64]struct{}
	dirty   bool
	stopped bool
}

// Observe registers obs to be notified after each presence change on this
// Awareness (a local SetLocalState/ClearLocalState or a remote ApplyUpdate).
// Notifications are delivered on a dedicated background goroutine with no locks
// held, carrying the sorted added/updated/removed client-id sets as JSON. The
// returned Subscription detaches the observer when Closed. If the Awareness is
// already Closed, a non-nil already-closed Subscription is returned (its Close
// is a no-op).
func (w *Awareness) Observe(obs AwarenessObserver) *Subscription {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.a == nil {
		return closedSubscription()
	}
	p := &awarenessPending{
		added:   map[uint64]struct{}{},
		updated: map[uint64]struct{}{},
		removed: map[uint64]struct{}{},
	}
	p.cond = sync.NewCond(&p.mu)

	// Bridge callback: fires SYNCHRONOUSLY from inside SetLocalState /
	// ClearLocalState / ApplyUpdate while the calling mobile method holds
	// w.mu.RLock. It must be cheap — it only unions the id sets under p.mu. It
	// never takes w.mu, never calls the observer, never blocks.
	unsub := w.a.OnChange(func(ev awareness.ChangeEvent) {
		if len(ev.Added)+len(ev.Updated)+len(ev.Removed) == 0 {
			return // never wake the app for a no-op change
		}
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return
		}
		for _, id := range ev.Added {
			p.added[id] = struct{}{}
		}
		for _, id := range ev.Updated {
			p.updated[id] = struct{}{}
		}
		for _, id := range ev.Removed {
			p.removed[id] = struct{}{}
		}
		p.dirty = true
		p.cond.Signal()
		p.mu.Unlock()
	})

	sub := &Subscription{
		unsubscribe: unsub,
		stopFn: func() {
			p.mu.Lock()
			p.stopped = true
			p.cond.Broadcast()
			p.mu.Unlock()
		},
	}
	sub.detach = func() { w.detachSub(sub) }

	go awarenessDrain(p, obs)

	w.subsMu.Lock()
	if w.subs == nil {
		w.subs = make(map[*Subscription]struct{})
	}
	w.subs[sub] = struct{}{}
	w.subsMu.Unlock()
	return sub
}

// awarenessDrain delivers coalesced presence changes to obs, one batch at a
// time, with no locks held. It exits when the subscription is stopped,
// abandoning any still-pending batch (no callback after Close).
func awarenessDrain(p *awarenessPending, obs AwarenessObserver) {
	for {
		p.mu.Lock()
		for !p.dirty && !p.stopped {
			p.cond.Wait()
		}
		if p.stopped { // abandon the pending batch; no callback after Close
			p.mu.Unlock()
			return
		}
		ch := awarenessChanges{
			Added:   sortedIDs(p.added),
			Updated: sortedIDs(p.updated),
			Removed: sortedIDs(p.removed),
		}
		// Reset the batch. Fresh maps (rather than clearing) keep the drained
		// snapshot's slices independent of the next accumulation.
		p.added = map[uint64]struct{}{}
		p.updated = map[uint64]struct{}{}
		p.removed = map[uint64]struct{}{}
		p.dirty = false
		p.mu.Unlock()

		// Marshaling id slices never fails, so the error is ignored. Delivery is
		// off all locks.
		b, _ := json.Marshal(ch)
		obs.OnChange(b)
	}
}

// sortedIDs returns the map's keys as a NON-nil ascending slice (empty → an
// empty non-nil slice) so the JSON marshals to `[]`, never `null`.
func sortedIDs(m map[uint64]struct{}) []uint64 {
	out := make([]uint64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (w *Awareness) detachSub(s *Subscription) {
	w.subsMu.Lock()
	delete(w.subs, s)
	w.subsMu.Unlock()
}
