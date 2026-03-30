package crdt

import (
	"sort"

	"github.com/reearth/ygo/encoding"
)

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

// EncodeSnapshot serialises snap to bytes.
// Wire format: VarBytes(encodedStateVector) + VarBytes(encodedDeleteSet)
// This format is compatible with Y.encodeSnapshot / Y.decodeSnapshot in the
// JavaScript Yjs reference implementation.
func EncodeSnapshot(snap *Snapshot) []byte {
	enc := encoding.NewEncoder()

	// State vector block.
	svEnc := encoding.NewEncoder()
	clients := clientsSorted(snap.StateVector)
	svEnc.WriteVarUint(uint64(len(clients)))
	for _, c := range clients {
		svEnc.WriteVarUint(uint64(c))
		svEnc.WriteVarUint(snap.StateVector[c])
	}
	enc.WriteVarBytes(svEnc.Bytes())

	// Delete set block.
	dsEnc := encoding.NewEncoder()
	encodeDeleteSet(dsEnc, snap.DeleteSet)
	enc.WriteVarBytes(dsEnc.Bytes())

	return enc.Bytes()
}

// DecodeSnapshot parses bytes produced by EncodeSnapshot.
func DecodeSnapshot(data []byte) (*Snapshot, error) {
	dec := encoding.NewDecoder(data)

	svBytes, err := dec.ReadVarBytes()
	if err != nil {
		return nil, wrapUpdateErr(err)
	}
	sv, err := DecodeStateVectorV1(svBytes)
	if err != nil {
		return nil, err
	}

	dsBytes, err := dec.ReadVarBytes()
	if err != nil {
		return nil, wrapUpdateErr(err)
	}
	dsDec := encoding.NewDecoder(dsBytes)
	ds, err := decodeDeleteSet(dsDec)
	if err != nil {
		return nil, wrapUpdateErr(err)
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

// RestoreDocument creates a new Doc that reflects doc's state at the time snap
// was taken. Items inserted after the snapshot are excluded, and only deletions
// present in the snapshot's DeleteSet are applied.
//
// The original doc must still contain the full item history — either
// doc.GC was false, or RunGC has not yet discarded items relevant to the
// snapshot.
func RestoreDocument(doc *Doc, snap *Snapshot) (*Doc, error) {
	doc.mu.Lock()
	update := encodeFromSnapshotLocked(doc, snap)
	doc.mu.Unlock()

	newDoc := New(WithGC(false))
	return newDoc, ApplyUpdateV1(newDoc, update, nil)
}

// EncodeStateFromSnapshot returns a V1 update representing doc's state at snap
// time. Apply it to a fresh Doc to reconstruct the historical version.
func EncodeStateFromSnapshot(doc *Doc, snap *Snapshot) ([]byte, error) {
	doc.mu.Lock()
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
			encodeItem(enc, item, 0)
		}
	}

	// Use the snapshot's delete set, not the current document delete set.
	// This preserves items that were deleted after the snapshot was taken.
	encodeDeleteSet(enc, snap.DeleteSet)
	return enc.Bytes()
}

// RunGC replaces the content of deleted items with lightweight ContentDeleted
// tombstones, freeing memory while preserving the structural position information
// required for CRDT correctness. It then merges adjacent ContentDeleted items
// from the same client into single nodes, compacting the linked list.
//
// This is a no-op when doc.GC is false. After RunGC runs, RestoreDocument can
// no longer reconstruct states that predate the GC'd deletions — take snapshots
// before calling RunGC if you need to preserve history.
func RunGC(doc *Doc) {
	if !doc.GC {
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
