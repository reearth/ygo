package mobile

import (
	"bytes"
	"sync"

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
