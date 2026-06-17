package crdt

import (
	"sort"

	"github.com/reearth/ygo/encoding"
)

// This file implements struct-level merge / diff / state-vector extraction for
// V1 updates (and, in merge_v2.go, the V2 columnar format). The previous
// MergeUpdatesV1 / DiffUpdateV1 applied updates to a temporary Doc and
// re-encoded its INTEGRATED state, which silently dropped any struct whose
// dependency was missing (it parked in store.pending and was never emitted) —
// corrupting the common partial-diff case (#125). These functions instead
// operate at the struct level, like Yjs mergeUpdates / diffUpdate: decode every
// struct WITHOUT integrating, merge per client (dedup/slice overlaps, preserve
// clock gaps as skip structs), and re-encode — so non-integrable structs are
// preserved verbatim for a downstream consumer that already has their deps.

// decodeStructsV1 decodes every struct in a V1 update into per-client,
// clock-ordered slices (without integrating) plus the update's delete set.
// scratch is a throwaway Doc used only to resolve parent type names during
// per-struct decode; it is never mutated structurally.
func decodeStructsV1(scratch *Doc, update []byte) (map[ClientID][]*Item, DeleteSet, error) {
	dec := encoding.NewDecoder(update)
	numClients, err := dec.ReadVarUint()
	if err != nil {
		return nil, DeleteSet{}, wrapUpdateErr(err)
	}
	if numClients > maxV2Items {
		return nil, DeleteSet{}, ErrInvalidUpdate
	}
	// Don't pre-size by numClients: it's attacker-controlled (a tiny update can
	// claim up to maxV2Items clients), so a size hint would amplify allocation.
	// The map grows as real groups are read.
	out := make(map[ClientID][]*Item)
	total := uint64(0)
	for i := uint64(0); i < numClients; i++ {
		numStructs, err := dec.ReadVarUint()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		total += numStructs
		if total > maxV2Items {
			return nil, DeleteSet{}, ErrInvalidUpdate
		}
		clientU, err := dec.ReadVarUint()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		client := ClientID(clientU)
		clock, err := dec.ReadVarUint()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		structs := out[client]
		for j := uint64(0); j < numStructs; j++ {
			item, err := decodeItem(dec, scratch, client, clock)
			if err != nil {
				return nil, DeleteSet{}, wrapUpdateErr(err)
			}
			clock += uint64(item.Content.Len())
			// Skip structs are clock-range placeholders, not content. Drop them
			// from the struct list — advancing clock above leaves a gap that the
			// encoder re-emits as a skip (Yjs filterSkips). Carrying them would
			// later hand a contentSkip to encodeItem, which can't encode it.
			if _, isSkip := item.Content.(*contentSkip); isSkip {
				continue
			}
			structs = append(structs, item)
		}
		out[client] = structs
	}
	ds, err := decodeDeleteSet(dec)
	if err != nil {
		return nil, DeleteSet{}, wrapUpdateErr(err)
	}
	return out, ds, nil
}

// sliceItemFront mutates item to drop its first `offset` logical units, so it
// starts `offset` clocks later. Used to dedup overlapping struct ranges when
// merging. Content.Splice(offset) mutates the receiver to hold [0,offset) and
// returns the suffix; we keep the suffix.
func sliceItemFront(item *Item, offset int) {
	// An origin-less, parent-less struct (a GC placeholder) must stay so it
	// re-encodes as a GC struct rather than a normal item; only set Origin for a
	// real item (one that already has an origin or a resolved parent). Skip
	// structs never reach here — they're filtered out during decode.
	hadAnchor := item.Origin != nil || item.Parent != nil
	suffix := item.Content.Splice(offset)
	item.ID.Clock += uint64(offset)
	if hadAnchor {
		item.Origin = &ID{Client: item.ID.Client, Clock: item.ID.Clock - 1}
	}
	item.Content = suffix
}

// dedupClientStructs sorts a client's structs by clock and removes/slices
// overlaps so the result is non-overlapping and clock-ascending. Clock GAPS are
// preserved (the encoder emits skip structs for them).
func dedupClientStructs(items []*Item) []*Item {
	// Sort by clock, longest-first on ties, so the merge is deterministic when
	// two inputs carry overlapping ranges starting at the same clock (the longer
	// struct is kept and shorter duplicates are dropped/sliced consistently).
	sort.Slice(items, func(i, j int) bool {
		if items[i].ID.Clock != items[j].ID.Clock {
			return items[i].ID.Clock < items[j].ID.Clock
		}
		return items[i].Content.Len() > items[j].Content.Len()
	})
	out := items[:0]
	var covEnd uint64
	hasCov := false
	for _, it := range items {
		end := it.ID.Clock + uint64(it.Content.Len())
		if hasCov && end <= covEnd {
			continue // fully covered by an earlier struct
		}
		if hasCov && it.ID.Clock < covEnd {
			sliceItemFront(it, int(covEnd-it.ID.Clock))
		}
		out = append(out, it)
		covEnd = end
		hasCov = true
	}
	return out
}

// encodeStructStoreV1 encodes per-client struct slices (clock-ordered,
// non-overlapping) plus a delete set into a V1 update. Clock gaps within a
// client are filled with skip structs (info tag 10). sv filters out structs
// fully below the receiver's state. store provides origin look-ups for
// encodeItem's GC-placeholder check.
func encodeStructStoreV1(perClient map[ClientID][]*Item, ds DeleteSet, sv StateVector, store *StructStore) []byte {
	clients := make([]ClientID, 0, len(perClient))
	for c, items := range perClient {
		if len(items) > 0 {
			clients = append(clients, c)
		}
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })

	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		// First pass per client: build the emit sequence (skips for gaps +
		// items above sv) so we can write the exact struct count up front.
		type entry struct {
			item   *Item // nil => skip struct
			offset int
			skip   uint64
		}
		type group struct {
			client     ClientID
			startClock uint64
			entries    []entry
		}
		var groups []group
		for _, c := range clients {
			svClock := sv.Clock(c)
			items := perClient[c]
			var ents []entry
			startClock := uint64(0)
			started := false
			expected := uint64(0)
			for _, it := range items {
				end := it.ID.Clock + uint64(it.Content.Len())
				if end <= svClock {
					continue // already known to the receiver
				}
				offset := 0
				clk := it.ID.Clock
				if clk < svClock {
					offset = int(svClock - clk)
					clk = svClock
				}
				if !started {
					startClock = clk
					expected = clk
					started = true
				}
				if clk > expected {
					ents = append(ents, entry{skip: clk - expected})
				}
				ents = append(ents, entry{item: it, offset: offset})
				expected = end
			}
			if started {
				groups = append(groups, group{client: c, startClock: startClock, entries: ents})
			}
		}

		enc.WriteVarUint(uint64(len(groups)))
		for _, g := range groups {
			enc.WriteVarUint(uint64(len(g.entries)))
			enc.WriteVarUint(uint64(g.client))
			enc.WriteVarUint(g.startClock)
			for _, e := range g.entries {
				if e.item == nil {
					enc.WriteUint8(10) // skip struct
					enc.WriteVarUint(e.skip)
					continue
				}
				encodeItem(enc, e.item, e.offset, store)
			}
		}
		encodeDeleteSet(enc, ds)
	})
}

// buildMergeStore decodes every update into one merged set of per-client struct
// slices (deduped/sliced, gaps preserved) and the union of their delete sets,
// plus a StructStore holding every struct for origin look-ups during encode.
func buildMergeStore(updates [][]byte, decode func(*Doc, []byte) (map[ClientID][]*Item, DeleteSet, error)) (map[ClientID][]*Item, DeleteSet, *StructStore, error) {
	scratch := New()
	perClient := make(map[ClientID][]*Item)
	mergedDS := newDeleteSet()
	for _, u := range updates {
		structs, ds, err := decode(scratch, u)
		if err != nil {
			return nil, DeleteSet{}, nil, err
		}
		for c, items := range structs {
			perClient[c] = append(perClient[c], items...)
		}
		mergedDS.Merge(ds)
	}
	store := &StructStore{clients: make(map[ClientID][]*Item, len(perClient))}
	for c, items := range perClient {
		deduped := dedupClientStructs(items)
		perClient[c] = deduped
		store.clients[c] = deduped
	}
	resolveParents(store)
	return perClient, mergedDS, store, nil
}

// resolveParents fills in Parent (and inherited ParentSub) for origin-inferred
// structs whose origin item is present in the merged set. decodeItem leaves
// these nil because the structs are never integrated, but encodeItem treats a
// nil-Parent origin item as a GC placeholder and would clear the dependent's
// origin — breaking e.g. a YMap key's origin chain (#125). Resolving here
// (mirroring decodeItem's origin inheritance, against the full struct set)
// keeps the chain intact. Structs whose origin is NOT present (a diff that
// references prior state) keep a nil Parent and their origin on the wire, so
// the receiver infers the parent once it has the dependency.
func resolveParents(store *StructStore) {
	for changed := true; changed; {
		changed = false
		for _, items := range store.clients {
			for _, it := range items {
				if it.Parent != nil {
					continue
				}
				var ref *Item
				if it.Origin != nil {
					ref = store.Find(*it.Origin)
				} else if it.OriginRight != nil {
					ref = store.Find(*it.OriginRight)
				}
				if ref != nil && ref.Parent != nil {
					it.Parent = ref.Parent
					if it.ParentSub == nil {
						it.ParentSub = ref.ParentSub
					}
					changed = true
				}
			}
		}
	}
}

// MergeUpdatesV1 merges several V1 updates into one. Structs are merged at the
// struct level (preserving non-integrable structs and clock gaps) and delete
// sets are unioned — matching Yjs mergeUpdates. (Previously this applied to a
// temp Doc and re-encoded, silently dropping parked structs; #125.)
func MergeUpdatesV1(updates ...[]byte) ([]byte, error) {
	perClient, ds, store, err := buildMergeStore(updates, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV1(perClient, ds, StateVector{}, store), nil
}

// DiffUpdateV1 returns the portion of update that a peer with state vector sv is
// missing: structs whose clock range extends past sv are kept (sliced at the sv
// boundary), and the full delete set is preserved (deletions are small and
// idempotent — matching Yjs diffUpdate). (Previously this dropped parked
// structs; #125.)
func DiffUpdateV1(update []byte, sv StateVector) ([]byte, error) {
	perClient, ds, store, err := buildMergeStore([][]byte{update}, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV1(perClient, ds, sv, store), nil
}

// EncodeStateVectorFromUpdate extracts the state vector described by a V1 update
// — the next clock per client (max struct end) — without integrating it. Useful
// for computing what a peer is missing directly from an update. (#57)
func EncodeStateVectorFromUpdate(update []byte) ([]byte, error) {
	perClient, _, _, err := buildMergeStore([][]byte{update}, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	sv := make(StateVector, len(perClient))
	for c, items := range perClient {
		var maxEnd uint64
		for _, it := range items {
			if end := it.ID.Clock + uint64(it.Content.Len()); end > maxEnd {
				maxEnd = end
			}
		}
		sv[c] = maxEnd
	}
	clients := clientsSorted(sv)
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(len(clients)))
		for _, c := range clients {
			enc.WriteVarUint(uint64(c))
			enc.WriteVarUint(sv[c])
		}
	}), nil
}
