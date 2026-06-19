package crdt

import (
	"errors"
	"sort"

	"github.com/reearth/ygo/encoding"
)

// ErrSnapshotSourceGCed is returned by CreateDocFromSnapshot / RestoreDocument
// when the source doc has garbage collection enabled. A GC-enabled doc replaces
// the content of deleted items with length-only tombstones at transaction
// commit (#78 H1), so an item deleted AFTER the snapshot no longer carries the
// content it had at snapshot time — reconstruction would silently produce a
// wrong (incomplete) document. Create the source with WithGC(false) to preserve
// the history a snapshot needs.
var ErrSnapshotSourceGCed = errors.New("crdt: cannot reconstruct from a GC-enabled source doc; create it with WithGC(false)")

// Snapshot captures the state of a Yjs document at a specific moment in time.
// It records which items existed (StateVector) and which were deleted (DeleteSet)
// at that moment. Snapshots can be used to restore documents to a past state or
// to compute what has changed between two points in time.
type Snapshot struct {
	StateVector StateVector
	DeleteSet   DeleteSet
}

// CaptureSnapshot takes a snapshot of doc's current state.
func CaptureSnapshot(doc *Doc) *Snapshot {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	return &Snapshot{
		StateVector: doc.store.StateVector(),
		DeleteSet:   buildDeleteSet(doc.store),
	}
}

// EncodeSnapshot serialises snap to bytes in the Yjs V1 snapshot format,
// interoperable with Y.encodeSnapshot / Y.decodeSnapshot.
//
// Wire layout (matching Yjs encodeSnapshotV2 with a V1 encoder): the delete set
// FIRST, then the state vector, concatenated into a single stream with NO outer
// length prefixes — `writeDeleteSet(enc, ds); writeStateVector(enc, sv)`. The
// previous ygo layout wrote the state vector first, each block wrapped in its own
// VarBytes length prefix, which no Yjs peer could parse (review finding F-5).
func EncodeSnapshot(snap *Snapshot) []byte {
	enc := encoding.NewEncoder()
	encodeDeleteSet(enc, snap.DeleteSet)
	encodeSnapshotStateVector(enc, snap.StateVector)
	return enc.Bytes()
}

// encodeSnapshotStateVector writes the state vector inline (no length prefix),
// matching Yjs writeStateVector: count, then (client, clock) pairs.
func encodeSnapshotStateVector(enc *encoding.Encoder, sv StateVector) {
	clients := clientsSorted(sv)
	enc.WriteVarUint(uint64(len(clients)))
	for _, c := range clients {
		enc.WriteVarUint(uint64(c))
		enc.WriteVarUint(sv[c])
	}
}

// DecodeSnapshot parses bytes produced by EncodeSnapshot (or Y.encodeSnapshot):
// delete set first, then the state vector, both inline (no length prefixes).
func DecodeSnapshot(data []byte) (*Snapshot, error) {
	dec := encoding.NewDecoder(data)

	ds, err := decodeDeleteSet(dec)
	if err != nil {
		return nil, wrapUpdateErr(err)
	}

	n, err := dec.ReadVarUint()
	if err != nil {
		return nil, wrapUpdateErr(err)
	}
	// Each entry requires at least 2 bytes (client varuint + clock varuint).
	// Guard against a crafted count that would force a huge map allocation
	// before the (then-failing) reads — same protection as DecodeStateVectorV1.
	if n > uint64(dec.Remaining()/2) || n > maxV2Items {
		return nil, wrapUpdateErr(ErrInvalidUpdate)
	}
	sv := make(StateVector, n)
	for i := uint64(0); i < n; i++ {
		client, err := dec.ReadVarUint()
		if err != nil {
			return nil, wrapUpdateErr(err)
		}
		clock, err := dec.ReadVarUint()
		if err != nil {
			return nil, wrapUpdateErr(err)
		}
		sv[ClientID(client)] = clock
	}

	return &Snapshot{StateVector: sv, DeleteSet: ds}, nil
}

// EqualSnapshots reports whether a and b represent exactly the same state.
func EqualSnapshots(a, b *Snapshot) bool {
	if len(a.StateVector) != len(b.StateVector) {
		return false
	}
	for client, clock := range a.StateVector {
		if b.StateVector[client] != clock {
			return false
		}
	}
	if len(a.DeleteSet.clients) != len(b.DeleteSet.clients) {
		return false
	}
	for client, aRanges := range a.DeleteSet.clients {
		bRanges := b.DeleteSet.clients[client]
		if len(aRanges) != len(bRanges) {
			return false
		}
		for i, r := range aRanges {
			if r != bRanges[i] {
				return false
			}
		}
	}
	return true
}

// CreateDocFromSnapshot reconstructs the document state captured by snap into a
// new, independent Doc — the Go equivalent of Yjs JS's createDocFromSnapshot.
// Items inserted after the snapshot are excluded, and only deletions present in
// the snapshot's DeleteSet are applied; a key/element deleted after the snapshot
// reappears in the reconstruction.
//
// src must have been created WithGC(false) (the snapshot-history pattern):
// a GC-enabled source may have discarded the content/tombstones the snapshot
// references, so reconstruction returns ErrSnapshotSourceGCed rather than a
// silently-wrong doc. The returned doc is itself non-GC, so it can be
// snapshotted or restored from again.
//
// Do not call from inside a Transact callback: it acquires src's lock and would
// deadlock — resolve the snapshot outside the transaction.
func CreateDocFromSnapshot(src *Doc, snap *Snapshot) (*Doc, error) {
	src.mu.Lock()
	if src.gc {
		src.mu.Unlock()
		return nil, ErrSnapshotSourceGCed
	}
	update := encodeFromSnapshotLocked(src, snap)
	src.mu.Unlock()

	newDoc := New(WithGC(false))
	if err := ApplyUpdateV1(newDoc, update, nil); err != nil {
		return nil, err
	}
	return newDoc, nil
}

// RestoreDocument creates a new Doc that reflects doc's state at the time snap
// was taken. Items inserted after the snapshot are excluded, and only deletions
// present in the snapshot's DeleteSet are applied.
//
// Retained for backward compatibility; it delegates to CreateDocFromSnapshot
// (the Yjs-parity name) and shares its GC-safety guard — see
// ErrSnapshotSourceGCed.
func RestoreDocument(doc *Doc, snap *Snapshot) (*Doc, error) {
	return CreateDocFromSnapshot(doc, snap)
}

// EncodeStateFromSnapshot returns a V1 update representing doc's state at snap
// time. Apply it to a fresh Doc to reconstruct the historical version.
//
// Like CreateDocFromSnapshot, doc must have been created WithGC(false): a
// GC-enabled source may have discarded content the snapshot references, so this
// returns ErrSnapshotSourceGCed rather than a silently-incomplete export. Do not
// call it from inside a Transact callback — it takes the doc lock and would
// deadlock.
func EncodeStateFromSnapshot(doc *Doc, snap *Snapshot) ([]byte, error) {
	doc.mu.Lock()
	if doc.gc {
		doc.mu.Unlock()
		return nil, ErrSnapshotSourceGCed
	}
	update := encodeFromSnapshotLocked(doc, snap)
	doc.mu.Unlock()
	return update, nil
}

// encodeFromSnapshotLocked builds a V1 update containing only items within
// snap.StateVector, encoded with snap.DeleteSet as the delete set.
// This correctly omits post-snapshot insertions and post-snapshot deletions.
// Must be called with doc.mu held.
func encodeFromSnapshotLocked(doc *Doc, snap *Snapshot) []byte {
	enc := encoding.NewEncoder()

	type clientGroup struct {
		client ClientID
		items  []*Item
	}

	var groups []clientGroup
	for client, items := range doc.store.clients {
		snapClock := snap.StateVector.Clock(client)
		var relevant []*Item
		for _, item := range items {
			// Include items whose starting clock falls within the snapshot window.
			// StateVector clocks are always at item boundaries, so no partial overlap.
			if item.ID.Clock < snapClock {
				relevant = append(relevant, item)
			}
		}
		if len(relevant) > 0 {
			groups = append(groups, clientGroup{client, relevant})
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].client < groups[j].client })

	enc.WriteVarUint(uint64(len(groups)))
	for _, g := range groups {
		enc.WriteVarUint(uint64(len(g.items)))
		enc.WriteVarUint(uint64(g.client))
		enc.WriteVarUint(0) // startClock = 0 (encoding from the beginning)
		for _, item := range g.items {
			encodeItem(enc, item, 0, doc.store)
		}
	}

	// Use the snapshot's delete set, not the current document delete set.
	// This preserves items that were deleted after the snapshot was taken.
	encodeDeleteSet(enc, snap.DeleteSet)
	return enc.Bytes()
}

// gcTxnDeleteSet is the per-transaction counterpart to RunGC (#78 H1, v1.15.0).
// Called from transactInternal under the doc lock, after buildPhase2 has
// captured observer deltas. For every item tombstoned in this transaction,
// replaces its Content with a length-only ContentDeleted placeholder so
// long-running documents don't retain full content for items that will
// never be observable again.
//
// Caller must hold d.mu. Unlike RunGC this does NOT acquire the lock or
// run the adjacent-tombstone merge pass — those are for the manual
// whole-document RunGC entry point.
func gcTxnDeleteSet(doc *Doc, txn *Transaction) {
	for client, ranges := range txn.deleteSet.clients {
		items := doc.store.clients[client]
		if len(items) == 0 {
			continue
		}
		for _, r := range ranges {
			rangeEnd := r.Clock + r.Len
			// Skip past items whose end is before the range start.
			for _, item := range items {
				if item.ID.Clock >= rangeEnd {
					break
				}
				if item.ID.Clock+uint64(item.Content.Len()) <= r.Clock {
					continue
				}
				if !item.Deleted {
					continue // shouldn't happen for items in deleteSet, but defensive
				}
				if _, alreadyGC := item.Content.(*ContentDeleted); alreadyGC {
					continue
				}
				item.Content = NewContentDeleted(item.Content.Len())
			}
		}
	}
}

// RunGC replaces the content of deleted items with lightweight ContentDeleted
// tombstones, freeing memory while preserving the structural position information
// required for CRDT correctness. It then merges adjacent ContentDeleted items
// from the same client into single nodes, compacting the linked list.
//
// This is a no-op when doc.gc is false. After RunGC runs, RestoreDocument can
// no longer reconstruct states that predate the GC'd deletions — take snapshots
// before calling RunGC if you need to preserve history.
func RunGC(doc *Doc) {
	if !doc.gc {
		return
	}
	doc.mu.Lock()
	defer doc.mu.Unlock()

	for client, items := range doc.store.clients {
		// Pass 1: replace deleted item content with ContentDeleted tombstones.
		for _, item := range items {
			if item.Deleted {
				if _, alreadyGC := item.Content.(*ContentDeleted); !alreadyGC {
					item.Content = NewContentDeleted(item.Content.Len())
				}
			}
		}

		// Pass 2: merge adjacent ContentDeleted items that are consecutive in
		// both the store slice and the linked list. Merging them reduces the
		// number of nodes future origin lookups must traverse.
		kept := make([]*Item, 0, len(items))
		for _, item := range items {
			prevCD, prevIsCDItem := func() (*ContentDeleted, bool) {
				if len(kept) == 0 {
					return nil, false
				}
				p := kept[len(kept)-1]
				cd, ok := p.Content.(*ContentDeleted)
				return cd, ok
			}()
			itemCD, itemIsCD := item.Content.(*ContentDeleted)

			// Merge only when both are tombstones, directly adjacent in the
			// linked list (no gap, no interleaving items), and clocks are
			// contiguous (prev.Clock+prev.Len == item.Clock).
			prev := func() *Item {
				if len(kept) == 0 {
					return nil
				}
				return kept[len(kept)-1]
			}()
			if prevIsCDItem && itemIsCD &&
				prev.Right == item && item.Left == prev &&
				prev.ID.Clock+uint64(prev.Content.Len()) == item.ID.Clock {
				// Absorb item into prev: extend the tombstone length, rewire
				// the linked list, and drop item from the store slice.
				prevCD.length += itemCD.length
				prev.Right = item.Right
				if item.Right != nil {
					item.Right.Left = prev
				}
				// item is discarded from kept — it no longer exists as a
				// separate node.
				continue
			}
			kept = append(kept, item)
		}
		doc.store.clients[client] = kept
	}
}
