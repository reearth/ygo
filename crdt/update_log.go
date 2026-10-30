package crdt

import (
	"cmp"
	"slices"
	"sync"
)

// LoggedUpdate is one committed transaction's update, what it parked, and its
// origin.
//
// Update is the V1 update the transaction's OnUpdate observers are handed,
// the very slice. Like Yjs's update event, it leaves out structs and
// deletions the transaction parked because they wait for something the
// document lacks. Parked is a V1 update of those, nil when the transaction
// parked nothing. A reader that keeps a copy of the document applies Update
// and then Parked; one that only follows what integrated reads Update alone.
type LoggedUpdate struct {
	Update []byte
	Parked []byte
	Origin any
}

// UpdateLog holds a document's committed transactions' updates in the order
// the transactions committed.
//
// OnUpdate cannot give that order. Observers run after the transaction
// releases the document lock (see Transact), so the observers of two
// transactions that commit close together, on different goroutines, can run
// in either order: an observer that renders or classifies each update against
// a replica cannot tell whether a later commit's observer has already caught
// the replica up. An UpdateLog is appended to under the document lock, so
// each observer that drains it finds every update committed so far, in commit
// order, its own included.
//
// The log grows until it is drained: a reader that opens one must Drain it
// (typically from an OnUpdate observer) or Close it.
//
// An UpdateLog is safe for concurrent use.
type UpdateLog struct {
	doc *Doc

	// mu guards the fields below. A committing transaction takes it while
	// it holds doc.mu, so the lock order is doc.mu, then mu; Drain, Pending
	// and Close take mu alone.
	mu       sync.Mutex
	closed   bool
	wantBase bool   // the next transaction to commit records base
	base     []byte // the recorded base, until a Drain hands it over
	updates  []LoggedUpdate
}

// OpenUpdateLog starts logging d. Every transaction that commits after it
// returns is appended to the log under d's lock, before d is unlocked, so
// before any observer of that transaction runs: an observer that drains the
// log finds its own transaction's update there, after every update committed
// before it, whatever order the observers of concurrent transactions run in.
// A transaction is logged exactly when OnUpdate observers would be called for
// it, empty and panicking transactions included, with the very bytes those
// observers receive, and what it parked beside them (LoggedUpdate).
//
// The first transaction to commit after the log opens also records d's
// whole state as of just before that transaction: a V1 update, what
// EncodeStateAsUpdateV1(d, nil) would return at that moment, parked structs
// and deletions included. It is captured under d's lock before the
// transaction's fn runs, so a reader that applies that base and then each
// logged update's Update and Parked in order holds d as it stood after each
// commit in turn, including what d holds parked: a replica rebuilt from the
// log integrates a parked struct when its missing predecessor arrives, as d
// does. A document that never commits after the log opens records no base.
//
// Like the observer registration methods, OpenUpdateLog takes d's lock and
// must not be called from inside a Transact callback.
func (d *Doc) OpenUpdateLog() *UpdateLog {
	l := &UpdateLog{doc: d, wantBase: true}
	d.mu.Lock()
	d.updateLogs = append(d.updateLogs, l)
	d.mu.Unlock()
	return l
}

// Drain hands over what has been logged since the last Drain, in commit
// order: the base, on the first Drain after it is recorded and never again
// (nil otherwise), and the updates. It does not take d's lock, so it may be
// called from anywhere, an observer included.
func (l *UpdateLog) Drain() (base []byte, updates []LoggedUpdate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	base, updates = l.base, l.updates
	l.base, l.updates = nil, nil
	return base, updates
}

// Pending is the number of logged updates no Drain has handed over yet.
func (l *UpdateLog) Pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.updates)
}

// Reset hands over every logged update no Drain has taken, drops a recorded
// base no Drain has taken, and makes the next transaction to commit record a
// fresh base, so a reader whose replica can no longer follow the log can
// start again from that base. It runs under d's lock, so every transaction
// commits either before Reset, and its update is in the returned slice (or
// was drained before), or after it, and is logged after the fresh base.
//
// Reset takes d's lock: call it from an observer or any other goroutine, but
// never from inside a Transact callback. On a closed log it returns nil.
func (l *UpdateLog) Reset() []LoggedUpdate {
	l.doc.mu.Lock()
	defer l.doc.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	updates := l.updates
	l.base, l.updates = nil, nil
	l.wantBase = !l.closed
	return updates
}

// Close stops logging and drops whatever is logged. It does not take d's
// lock; the document forgets the log at its next commit. Close is safe to
// call more than once and concurrently with commits and Drain.
func (l *UpdateLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.wantBase = false
	l.base, l.updates = nil, nil
}

// needsBase reports whether l waits for a base.
func (l *UpdateLog) needsBase() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wantBase
}

// setBase records base unless l was closed meanwhile.
func (l *UpdateLog) setBase(base []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wantBase {
		l.base = base
		l.wantBase = false
	}
}

// recordUpdateLogBases records d's whole state as the base of every open
// update log that waits for one. transactInternal calls it under d.mu before
// fn runs, so the base is the state just before the transaction. The state is
// encoded outside any log's lock, and once however many logs wait.
func (d *Doc) recordUpdateLogBases() {
	var base []byte
	for _, l := range d.updateLogs {
		if !l.needsBase() {
			continue
		}
		if base == nil {
			base = encodeStateAsUpdateV1Locked(d, nil)
		}
		l.setBase(base)
	}
}

// wantsUpdateBytes reports whether a commit must encode its update: for its
// OnUpdate observers or for an update log. The caller holds d.mu.
func (d *Doc) wantsUpdateBytes() bool {
	return len(d.onUpdate) > 0 || len(d.updateLogs) > 0
}

// parkedUpdate is a V1 update of what txn parked and still holds parked at
// its commit, or nil when it parked nothing: the structs and deletions
// encodeV1Locked leaves out, so that a log's base plus its updates holds what
// a logged apply parked, and a replica rebuilt from the log integrates it
// once the missing predecessor arrives. The structs come from one update, so
// each client's are disjoint; they are encoded whole (no state vector), as
// the base's are, so a reader parks each exactly as d did. The caller holds
// d.mu.
func parkedUpdate(d *Doc, txn *Transaction) []byte {
	if len(txn.parkedItems) == 0 && len(txn.parkedDeletes.clients) == 0 {
		return nil
	}
	s := d.store
	perClient := make(map[ClientID][]*Item)
	for _, it := range txn.parkedItems {
		if it.ID.Clock+uint64(it.Content.Len()) > s.NextClock(it.ID.Client) {
			perClient[it.ID.Client] = append(perClient[it.ID.Client], it)
		}
	}
	for _, items := range perClient {
		slices.SortFunc(items, func(a, b *Item) int { return cmp.Compare(a.ID.Clock, b.ID.Clock) })
	}
	if len(perClient) == 0 && len(txn.parkedDeletes.clients) == 0 {
		return nil
	}
	ds := txn.parkedDeletes
	if ds.clients == nil {
		ds = newDeleteSet()
	}
	return encodeStructStoreV1(perClient, ds, nil, s)
}

// appendUpdateLogs appends a committed transaction's update to every open
// update log and forgets the closed ones. buildPhase2 calls it under d.mu, so
// the logs receive updates in commit order. A log still waiting for its base
// here (only when encoding the base panicked) takes no update: its base,
// recorded by the next commit, already holds this one.
func (d *Doc) appendUpdateLogs(update, parked []byte, origin any) {
	open := d.updateLogs[:0]
	for _, l := range d.updateLogs {
		l.mu.Lock()
		if !l.closed {
			if !l.wantBase {
				l.updates = append(l.updates, LoggedUpdate{Update: update, Parked: parked, Origin: origin})
			}
			open = append(open, l)
		}
		l.mu.Unlock()
	}
	clear(d.updateLogs[len(open):])
	d.updateLogs = open
}
