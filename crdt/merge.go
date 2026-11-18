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
		start, err := readID(dec)
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		client, clock := start.Client, start.Clock
		// A later group for the client replaces earlier ones, as in apply (yjs parity).
		var structs []*Item
		for j := uint64(0); j < numStructs; j++ {
			item, err := decodeItem(dec, scratch, client, clock)
			if err != nil {
				return nil, DeleteSet{}, wrapUpdateErr(err)
			}
			if clock, err = structEnd(clock, item.Content.Len()); err != nil {
				return nil, DeleteSet{}, err
			}
			// Skip structs are clock-range placeholders, not content. Drop them
			// from the struct list — advancing clock above leaves a gap that the
			// encoder re-emits as a skip (Yjs filterSkips). Carrying them would
			// later hand a contentSkip to encodeItem, which can't encode it.
			// A struct of length zero is dropped too, so a merge cleans stored
			// state that holds one, which the apply refuses.
			if _, isSkip := item.Content.(*contentSkip); isSkip || item.Content.Len() == 0 {
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

// sliceItemFront drops item's first offset clocks, as Yjs sliceStruct does.
func sliceItemFront(item *Item, offset uint64) {
	// A GC placeholder keeps no origin so it re-encodes as a GC struct.
	hadAnchor := !item.isGCOrphan()
	suffix := item.Content.Splice(offset)
	item.ID.Clock += offset
	if hadAnchor {
		item.Origin = &ID{Client: item.ID.Client, Clock: item.ID.Clock - 1}
	}
	item.Content = suffix
}

// dedupClientStructs sorts a client's structs by clock and removes/slices
// overlaps so the result is non-overlapping and clock-ascending. Clock GAPS are
// preserved (the encoder emits skip structs for them). buildMergeStore hands
// it a client's structs only when no update carries one below the end of an
// earlier update's; mergeOverlapping takes the others.
func dedupClientStructs(items []*Item) []*Item {
	// Sort by clock, longest-first on ties, so the longer of two structs on
	// one clock is kept. mergeOverlapping sorts in this order too.
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
		end := it.ID.Clock + it.Content.Len()
		if hasCov && end <= covEnd {
			continue // fully covered by an earlier struct
		}
		if hasCov && it.ID.Clock < covEnd {
			sliceItemFront(it, covEnd-it.ID.Clock)
		}
		out = append(out, it)
		covEnd = end
		hasCov = true
	}
	return out
}

// carriedStruct is a struct an update carried, with its place among its
// client's structs in the order the updates carried them, and its clocks as
// carried.
type carriedStruct struct {
	item       *Item
	seq        int
	clock, end uint64
	done       bool // kept, or covered by kept structs
}

// mergeOverlapping is dedupClientStructs for a client whose updates carry
// structs over clocks that an earlier update's structs carry. items holds them
// in the order the updates carried them. Wherever a struct must start, the
// struct dedupClientStructs would keep is kept, cut there, unless it names
// there another origin, right origin, parent or key than the struct the
// earliest update carried among those holding that clock. The two are then
// different insertions, and the earlier update's struct is kept instead, as
// an apply of the updates in order integrates from a later struct only the
// clocks past those it holds. The struct that lost is cut at the end of the
// one kept, and the choice is made again there. So a struct that a later
// update carries over an earlier one's clocks and that names otherwise, such
// as one parked on a struct no update carries, keeps only the clocks past
// them.
func mergeOverlapping(items []*Item) []*Item {
	carried := make([]carriedStruct, len(items))
	for i, it := range items {
		carried[i] = carriedStruct{item: it, seq: i, clock: it.ID.Clock, end: it.ID.Clock + it.Content.Len()}
	}
	// dedupClientStructs' order, to the same permutation, so that this keeps
	// the struct it would keep wherever no earlier insertion names otherwise.
	sort.Slice(carried, func(i, j int) bool {
		if carried[i].clock != carried[j].clock {
			return carried[i].clock < carried[j].clock
		}
		return carried[i].end > carried[j].end
	})
	var covEnd uint64
	held := func(i int) bool { return carried[i].done || carried[i].end <= covEnd }
	// earliest holds, by the order the updates carried them, the structs that
	// start at or before covEnd; held ones leave it once at its top.
	earliest := candidateHeap{less: func(a, b int) bool { return carried[a].seq < carried[b].seq }}
	out := items[:0]
	first := 0 // the first struct in dedupClientStructs' order not held
	for next := 0; next < len(carried); {
		// The first struct, or one after a gap: as in dedupClientStructs, the
		// first there is kept even when it has length zero.
		covEnd = carried[next].clock
		if carried[next].end == covEnd {
			carried[next].done = true
			out = append(out, carried[next].item)
		}
		for {
			for ; next < len(carried) && carried[next].clock <= covEnd; next++ {
				earliest.push(next)
			}
			for first < next && held(first) {
				first++
			}
			if first == next {
				break
			}
			for held(earliest.idx[0]) {
				earliest.pop()
			}
			keep := first
			if e := earliest.idx[0]; e != first && !sameInsertionAt(carried[first].item, carried[e].item, covEnd) {
				keep = e
			}
			carried[keep].done = true
			it := carried[keep].item
			if it.ID.Clock < covEnd {
				sliceItemFront(it, covEnd-it.ID.Clock)
			}
			out = append(out, it)
			covEnd = carried[keep].end
		}
	}
	return out
}

// sameInsertionAt reports whether two structs that both hold clock at name
// the same origin there, and the same right origin, parent and key, as two
// copies of one insertion do. A struct that starts before at names there the
// clock before at, as it will once cut at at; a GC struct names nothing.
func sameInsertionAt(a, b *Item, at uint64) bool {
	ao, aHas := originAt(a, at)
	bo, bHas := originAt(b, at)
	return aHas == bHas && ao == bo && a.Parent == b.Parent && a.isGCOrphan() == b.isGCOrphan() &&
		originIDEquals(a.OriginRight, b.OriginRight) && originIDEquals(a.parentID, b.parentID) &&
		(a.ParentSub == nil) == (b.ParentSub == nil) && (a.ParentSub == nil || *a.ParentSub == *b.ParentSub)
}

// originAt is the origin it names at clock at, as sliceItemFront would set it.
func originAt(it *Item, at uint64) (ID, bool) {
	switch {
	case it.ID.Clock < at && !it.isGCOrphan():
		return ID{Client: it.ID.Client, Clock: at - 1}, true
	case it.Origin != nil:
		return *it.Origin, true
	}
	return ID{}, false
}

// candidateHeap is a binary heap of indexes into a merge's carried structs,
// least first.
type candidateHeap struct {
	idx  []int
	less func(a, b int) bool
}

func (h *candidateHeap) push(i int) {
	h.idx = append(h.idx, i)
	for c := len(h.idx) - 1; c > 0; {
		p := (c - 1) / 2
		if !h.less(h.idx[c], h.idx[p]) {
			break
		}
		h.idx[c], h.idx[p] = h.idx[p], h.idx[c]
		c = p
	}
}

func (h *candidateHeap) pop() {
	last := len(h.idx) - 1
	h.idx[0] = h.idx[last]
	h.idx = h.idx[:last]
	for p := 0; ; {
		least := p
		if l := 2*p + 1; l < len(h.idx) && h.less(h.idx[l], h.idx[least]) {
			least = l
		}
		if r := 2*p + 2; r < len(h.idx) && h.less(h.idx[r], h.idx[least]) {
			least = r
		}
		if least == p {
			return
		}
		h.idx[p], h.idx[least] = h.idx[least], h.idx[p]
		p = least
	}
}

// encodeStructStoreV1 encodes per-client struct slices (clock-ordered,
// non-overlapping) plus a delete set into a V1 update. Clock gaps within a
// client are filled with skip structs (info tag 10). sv filters out structs
// fully below the receiver's state.
func encodeStructStoreV1(perClient map[ClientID][]*Item, ds DeleteSet, sv StateVector) []byte {
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
			offset uint64
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
				end := it.ID.Clock + it.Content.Len()
				if end <= svClock {
					continue // already known to the receiver
				}
				offset := uint64(0)
				clk := it.ID.Clock
				if clk < svClock {
					offset = svClock - clk
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
				encodeItem(enc, e.item, e.offset)
			}
		}
		encodeDeleteSet(enc, ds)
	})
}

// buildMergeStore decodes every update into one merged set of per-client struct
// slices (deduped/sliced, gaps preserved) and the union of their delete sets.
func buildMergeStore(updates [][]byte, decode func(*Doc, []byte) (map[ClientID][]*Item, DeleteSet, error)) (map[ClientID][]*Item, DeleteSet, error) {
	scratch := New()
	perClient := make(map[ClientID][]*Item)
	// The clients an update carries structs of below the end of an earlier
	// update's. An update's structs of one client ascend without overlapping,
	// so every other client's structs ascend as carried.
	var overlapping map[ClientID]bool
	mergedDS := newDeleteSet()
	for _, u := range updates {
		structs, ds, err := decode(scratch, u)
		if err != nil {
			return nil, DeleteSet{}, err
		}
		for c, items := range structs {
			if have := perClient[c]; len(have) > 0 && len(items) > 0 {
				if last := have[len(have)-1]; items[0].ID.Clock < last.ID.Clock+last.Content.Len() {
					if overlapping == nil {
						overlapping = make(map[ClientID]bool)
					}
					overlapping[c] = true
				}
			}
			perClient[c] = append(perClient[c], items...)
		}
		mergedDS.Merge(ds)
	}
	for c, items := range perClient {
		if overlapping[c] {
			perClient[c] = mergeOverlapping(items)
		} else {
			perClient[c] = dedupClientStructs(items)
		}
	}
	return perClient, mergedDS, nil
}

// MergeUpdatesV1 merges several V1 updates into one. Structs are merged at the
// struct level (preserving non-integrable structs and clock gaps) and delete
// sets are unioned — matching Yjs mergeUpdates. (Previously this applied to a
// temp Doc and re-encoded, silently dropping parked structs; #125.)
func MergeUpdatesV1(updates ...[]byte) ([]byte, error) {
	perClient, ds, err := buildMergeStore(updates, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV1(perClient, ds, StateVector{}), nil
}

// DiffUpdateV1 returns the portion of update that a peer with state vector sv is
// missing: structs whose clock range extends past sv are kept (sliced at the sv
// boundary), and the full delete set is preserved (deletions are small and
// idempotent — matching Yjs diffUpdate). (Previously this dropped parked
// structs; #125.)
func DiffUpdateV1(update []byte, sv StateVector) ([]byte, error) {
	perClient, ds, err := buildMergeStore([][]byte{update}, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV1(perClient, ds, sv), nil
}

// EncodeStateVectorFromUpdate extracts the state vector described by a V1
// update without integrating it: each client's clock is the end of its
// structs from clock 0 up to the first Skip or gap, as Yjs reports it.
func EncodeStateVectorFromUpdate(update []byte) ([]byte, error) {
	return encodeStateVectorFromUpdate(update, false)
}

// encodeStateVectorFromUpdate reads the structs of a V1 or V2 update in wire
// order, as Yjs encodeStateVectorFromUpdate does, so a client's consecutive
// groups count as one run, a Skip of any length ends the run, and the delete
// set is never read.
func encodeStateVectorFromUpdate(update []byte, v2 bool) ([]byte, error) {
	type entry struct {
		client ClientID
		clock  uint64
	}
	var entries []entry
	var client ClientID
	var clock uint64
	started, stop := false, false
	s := newPendingScanner(update, v2)
	groups := s.uint()
	if groups > maxV2Items {
		return nil, ErrInvalidUpdate
	}
	var total uint64
	for i := uint64(0); i < groups && s.err == nil; i++ {
		n := s.uint()
		// The same bounds as the decoders, so a run-length V2 column cannot
		// declare more structs than an apply would read.
		if n > maxV2Items-total {
			return nil, ErrInvalidUpdate
		}
		total += n
		c := s.client()
		at := s.uint()
		if s.err == nil && c > maxClient {
			return nil, ErrInvalidUpdate
		}
		for j := uint64(0); j < n && s.err == nil; j++ {
			length, skip, _, _ := s.item()
			if s.err != nil {
				break
			}
			end, err := structEnd(at, length)
			if err != nil {
				return nil, err
			}
			switch {
			case !started:
				// Yjs starts the first run from the first struct, a Skip too.
				client, started, stop = c, true, at != 0
				if !stop {
					clock = end
				}
			case c != client:
				if clock != 0 {
					entries = append(entries, entry{client, clock})
				}
				client, clock, stop = c, 0, at != 0
			}
			if skip {
				stop = true
			}
			if !stop {
				clock = end
			}
			at = end
		}
	}
	if s.err != nil {
		return nil, wrapUpdateErr(scanReason(s.err))
	}
	if clock != 0 {
		entries = append(entries, entry{client, clock})
	}
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(len(entries)))
		for _, e := range entries {
			enc.WriteVarUint(uint64(e.client))
			enc.WriteVarUint(e.clock)
		}
	}), nil
}
