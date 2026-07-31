// Package relaylane provides the bounded per-room work queue shared by the
// cluster relay's inbound (cluster/redis subscriber) and outbound
// (provider/websocket publisher) delivery paths.
//
// # Why a lane per room
//
// Before #187 both directions funnelled every room through one goroutine, so
// one slow room stalled delivery for all of them. A Lane per room removes that
// coupling: producers hand off without blocking and each room's worker drains
// its own lane.
//
// # Full-lane policy: coalesce, never drop or block
//
// KindSync payloads are V1 update blobs, which are idempotent, commutative and
// mergeable. When the sync queue exceeds its cap the whole backlog is merged
// into a single blob via crdt.MergeUpdatesV1, freeing capacity without losing
// an edit. That is the only policy that is simultaneously lossless, bounded,
// and non-blocking — blocking the producer is the very bug #187 reports, and
// dropping diverges the cluster (a dropped update parks every later edit from
// that client until the room is reloaded, which a hot room never is).
//
// KindAwareness blobs are NOT mergeable and are kept latest-only. Awareness is
// idempotent heartbeat state, so a superseded entry self-heals within one
// heartbeat interval.
package relaylane

import (
	"sync"
	"sync/atomic"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// DefaultCap is the default number of queued KindSync blobs a Lane holds
// before it starts coalescing. Sized so ordinary edit bursts never coalesce
// while a wedged room's memory stays bounded.
const DefaultCap = 64

// Stats is a point-in-time snapshot of a Lane's degraded-path counters. A
// non-zero Coalesced means the lane has been saturated; a non-zero HardDrops
// means data was lost and the cluster may be diverged.
type Stats struct {
	// Coalesced counts KindSync updates absorbed into another blob by a
	// merge (n merged entries count as n-1).
	Coalesced uint64
	// AwarenessSuperseded counts awareness blobs replaced before delivery.
	AwarenessSuperseded uint64
	// HardDrops counts payloads lost outright. Should always be zero;
	// non-zero means MergeUpdatesV1 failed repeatedly on a saturated lane.
	HardDrops uint64
}

// Lane is a bounded work queue for one room. Safe for concurrent use: one
// producer (the transport) and one consumer (the room worker) is the intended
// pattern, but any number of either is safe.
//
// The degraded-path counters (coalesced/awarenessSuperseded/hardDrops) are
// plain atomics, deliberately NOT guarded by mu, even though every site that
// mutates them already holds mu for other reasons (the queue mutations they
// accompany). This is load-bearing: Stats() must be callable by a caller
// already holding some OTHER lock (see provider/websocket's
// Server.RelayStats() and cluster/redis's Relay.Stats(), both of which hold
// a server-level map lock across every live lane's Stats() call to
// guarantee monotonicity) without that call ever blocking on mu — Push and
// TakeSync both hold mu across a potentially slow crdt.MergeUpdatesV1 call,
// so a mutex-guarded Stats() could stall behind an in-flight merge on some
// OTHER room's lane while the caller holds its own lock, reintroducing
// exactly the cross-room coupling #187 removed (a slow room's merge would
// then transitively stall a stats poll for every room, since the map lock
// blocks new readers behind any pending writer). Atomics make Stats() lock-
// free: it cannot block on anything, ever, regardless of what any lane's
// Push/TakeSync is doing. Cross-field tearing between the three counters in
// one Stats() call is immaterial: nothing needs a single instant's
// consistent triple, only each field's own monotonic non-decrease.
type Lane struct {
	mu    sync.Mutex
	cap   int
	syncQ [][]byte
	aw    []byte
	hasAw bool

	coalesced           atomic.Uint64
	awarenessSuperseded atomic.Uint64
	hardDrops           atomic.Uint64

	signal chan struct{}
}

// New returns a Lane holding up to capacity KindSync blobs before coalescing.
// A non-positive capacity uses DefaultCap.
func New(capacity int) *Lane {
	if capacity <= 0 {
		capacity = DefaultCap
	}
	return &Lane{cap: capacity, signal: make(chan struct{}, 1)}
}

// Push enqueues a payload. It NEVER blocks: an over-cap sync queue is
// collapsed by merging, and awareness is kept latest-only. data must not be
// mutated by the caller afterwards — the Lane retains the slice.
func (l *Lane) Push(kind cluster.Kind, data []byte) {
	l.mu.Lock()
	if kind == cluster.KindAwareness {
		if l.hasAw {
			l.awarenessSuperseded.Add(1)
		}
		l.aw, l.hasAw = data, true
	} else {
		l.syncQ = append(l.syncQ, data)
		if len(l.syncQ) > l.cap {
			l.collapseLocked()
		}
	}
	l.mu.Unlock()
	l.notify()
}

// collapseLocked merges the whole sync backlog into one blob. On merge
// failure the backlog is left intact (one entry over cap) rather than losing
// data; only if it grows to twice the cap is the oldest entry dropped, which
// is the last resort and is counted. This can repeat on EVERY push once the
// backlog is over cap and MergeUpdatesV1 keeps failing: nothing here clears
// syncQ on failure, so the next push is still over cap and re-invokes this
// same merge attempt — including forever, once past 2*cap, one hard-drop per
// push. There is no point past which the storm self-resolves; it only stops
// if MergeUpdatesV1 starts succeeding again (see enqueueRelayOutbound's doc
// in provider/websocket/cluster.go for the amortized-cost analysis this
// degenerate case is the exception to).
func (l *Lane) collapseLocked() {
	merged, err := crdt.MergeUpdatesV1(l.syncQ...)
	if err != nil {
		if len(l.syncQ) > 2*l.cap {
			l.syncQ = l.syncQ[1:]
			l.hardDrops.Add(1)
		}
		return
	}
	l.coalesced.Add(uint64(len(l.syncQ) - 1))
	l.syncQ = [][]byte{merged}
}

// TakeSync removes and returns the pending KindSync work as a single blob,
// merging the backlog when more than one entry is queued. A lone entry is
// returned as-is so the fast path pays no merge cost. Reports false when
// there is no sync work pending.
func (l *Lane) TakeSync() ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch len(l.syncQ) {
	case 0:
		return nil, false
	case 1:
		b := l.syncQ[0]
		l.syncQ = l.syncQ[:0]
		return b, true
	}

	merged, err := crdt.MergeUpdatesV1(l.syncQ...)
	if err != nil {
		// Never lose the batch to a failed merge: fall back to delivering
		// the oldest entry on its own and retry the rest next call.
		b := l.syncQ[0]
		l.syncQ = l.syncQ[1:]
		return b, true
	}
	l.coalesced.Add(uint64(len(l.syncQ) - 1))
	l.syncQ = l.syncQ[:0]
	return merged, true
}

// TakeAwareness removes and returns the pending awareness blob, if any.
func (l *Lane) TakeAwareness() ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.hasAw {
		return nil, false
	}
	b := l.aw
	l.aw, l.hasAw = nil, false
	return b, true
}

// Signal is readable whenever work may be pending. It is a coalescing
// notification (capacity 1), so a worker must drain with TakeSync /
// TakeAwareness until both report false rather than assuming one signal
// equals one payload.
func (l *Lane) Signal() <-chan struct{} { return l.signal }

// Empty reports whether the lane holds no pending work.
func (l *Lane) Empty() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.syncQ) == 0 && !l.hasAw
}

// Stats returns a snapshot of the degraded-path counters. Lock-free by
// design (see the counters' doc on the Lane struct): it never acquires mu,
// so it cannot block behind Push/collapseLocked/TakeSync's crdt.MergeUpdatesV1
// call, however slow that call is. The three fields are read independently
// (three separate atomic loads, not one consistent snapshot) — callers only
// need each field's own monotonic non-decrease, never a single instant's
// consistent triple.
func (l *Lane) Stats() Stats {
	return Stats{
		Coalesced:           l.coalesced.Load(),
		AwarenessSuperseded: l.awarenessSuperseded.Load(),
		HardDrops:           l.hardDrops.Load(),
	}
}

func (l *Lane) notify() {
	select {
	case l.signal <- struct{}{}:
	default: // a signal is already pending; the worker will drain everything
	}
}
