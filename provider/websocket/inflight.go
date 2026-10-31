package websocket

import (
	"context"
	"sync"
	"sync/atomic"
)

// inFlight counts goroutines inside a section that Shutdown joins, such as a
// served connection or a commit on its way to the persistence adapter.
//
// zero is a broadcast latch, replaced and closed on each 1→0 transition. A
// close rather than a token hand-off, because Shutdown may be called
// concurrently (shutdownOnce implies as much) and a single wake token would be
// consumed by one caller, parking the others until their contexts expired. A
// plain sync.WaitGroup is unusable because producers appear at arbitrary
// times, so Add would race Wait. zero is nil on a zero-value Server, where
// nothing waits.
type inFlight struct {
	n    atomic.Int64
	mu   sync.Mutex // never taken by enter; by leave only on a 1→0 transition
	zero chan struct{}
}

// newInFlight returns a counter whose wait blocks while the count is above
// zero.
func newInFlight() inFlight { return inFlight{zero: make(chan struct{})} }

func (f *inFlight) enter() { f.n.Add(1) }

// leave balances enter and releases every waiter when this was the last
// goroutine in flight.
func (f *inFlight) leave() {
	if f.n.Add(-1) != 0 {
		return
	}
	f.mu.Lock()
	ch := f.zero
	if ch == nil {
		f.mu.Unlock()
		return
	}
	f.zero = make(chan struct{})
	f.mu.Unlock()
	close(ch)
}

// wait blocks until the count is zero or ctx expires.
//
// Latch-then-check is the order that makes this free of lost wakeups: the latch
// is read BEFORE the counter, so any 1→0 transition after the read closes the
// very channel this goroutine waits on, and any transition before it is caught
// by the counter read.
func (f *inFlight) wait(ctx context.Context) {
	for {
		f.mu.Lock()
		ch := f.zero
		f.mu.Unlock()
		if ch == nil || f.n.Load() == 0 {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}
