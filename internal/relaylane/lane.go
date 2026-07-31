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
type Lane struct {
	mu     sync.Mutex
	cap    int
	syncQ  [][]byte
	aw     []byte
	hasAw  bool
	stats  Stats
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
			l.stats.AwarenessSuperseded++
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
// is the last resort and is counted.
func (l *Lane) collapseLocked() {
	merged, err := crdt.MergeUpdatesV1(l.syncQ...)
	if err != nil {
		if len(l.syncQ) > 2*l.cap {
			l.syncQ = l.syncQ[1:]
			l.stats.HardDrops++
		}
		return
	}
	l.stats.Coalesced += uint64(len(l.syncQ) - 1)
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
	l.stats.Coalesced += uint64(len(l.syncQ) - 1)
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

// Stats returns a snapshot of the degraded-path counters.
func (l *Lane) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

func (l *Lane) notify() {
	select {
	case l.signal <- struct{}{}:
	default: // a signal is already pending; the worker will drain everything
	}
}
