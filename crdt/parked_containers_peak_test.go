package crdt

import (
	"errors"
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// The tests in this file hold each list and map that the parked content and
// the parked delete ranges keep to the rule the record of parked structs
// follows (shrinkParkedBy): one compacted or cut in place, or emptied, keeps
// no slots for the most it held.

// servedRoom is a document set up as a served room is for what it parks:
// caps of 4,096 parked structs and delete ranges, a budget that refuses
// nothing, and refused drains discarded, so the document records who parked
// each struct and range.
func servedRoom() *Doc {
	doc := New(WithClientID(1), WithMaxPendingItems(4096), WithMaxPendingDeleteRanges(4096), WithApplyBudget(unboundedBudget))
	doc.SetDiscardRefusedDrain(true)
	return doc
}

// bridgedRanges is a room that parked 4,096 one-clock delete ranges of client
// 9 at even clocks, from one sender, and then a frame from it whose range
// covers them and the clocks between, so the parked ranges merge into one.
func bridgedRanges(t *testing.T) *Doc {
	t.Helper()
	doc := servedRoom()
	sender := &writeOrigin{written: []byte{1}}
	ranges := make([]DeleteRange, 0, 4096)
	for k := range 4096 {
		ranges = append(ranges, DeleteRange{Clock: uint64(2 * k), Len: 1})
	}
	require.NoError(t, ApplyUpdateV1(doc, deleteSetUpdate(map[ClientID][]DeleteRange{9: ranges}), sender))
	require.NoError(t, ApplyUpdateV1(doc, deleteSetUpdate(map[ClientID][]DeleteRange{9: {{Clock: 0, Len: 2 * 4096}}}), sender))
	return doc
}

// spareRanges is the bytes r's array holds past its length.
func spareRanges(r []DeleteRange) int {
	return (cap(r) - len(r)) * int(unsafe.Sizeof(DeleteRange{}))
}

// A list compacted in place keeps at most three spare slots for the one range
// it holds: 48 bytes.
const spareRangesBound = 3 * int(unsafe.Sizeof(DeleteRange{}))

func TestUnit_DiscardRefusedDrain_ARecordedSetsRangesKeepNoSlotsForTheirPeak(t *testing.T) {
	doc := bridgedRanges(t)
	require.Len(t, doc.parkedDsBy, 1)
	r := doc.parkedDsBy[0].ds.clients[9]
	require.Len(t, r, 1)
	t.Logf("the recorded set's ranges: len %d cap %d, %d spare bytes", len(r), cap(r), spareRanges(r))
	require.LessOrEqual(t, spareRanges(r), spareRangesBound, "the record of who parked each range keeps the slots of its peak")
}

func TestUnit_PendingDeletes_AClientsRangesKeepNoSlotsForTheirPeak(t *testing.T) {
	doc := bridgedRanges(t)
	r := doc.store.pendingDs.clients[9]
	require.Len(t, r, 1)
	t.Logf("the pending deletes of client 9: len %d cap %d, %d spare bytes", len(r), cap(r), spareRanges(r))
	require.LessOrEqual(t, spareRanges(r), spareRangesBound, "the pending deletes keep the slots of their peak")
}

// parkedClients is how many clients the tests below park one delete range
// of each.
const parkedClients = 1024

// A map of the pending deletes' clients holds at most 4 KiB more than one
// made at its size (a map of 1,024 such clients holds about 70 KB).
const spareClientsBound = 4 << 10

// parkOneRangeEach parks on doc one delete range of each of parkedClients
// clients, and returns one update that gives each of them the struct its
// range deletes, but for the first without clients.
func parkOneRangeEach(t *testing.T, doc *Doc, without int) []byte {
	t.Helper()
	sets := make(map[ClientID][]DeleteRange, parkedClients)
	updates := make([][]byte, 0, parkedClients)
	for c := range parkedClients {
		client := ClientID(100 + c)
		sets[client] = []DeleteRange{{Clock: 0, Len: 1}}
		if c < without {
			continue
		}
		w := New(WithClientID(client))
		text := w.GetText("t")
		w.Transact(func(txn *Transaction) { text.Insert(txn, 0, "x", nil) })
		updates = append(updates, EncodeStateAsUpdateV1(w, nil))
	}
	require.NoError(t, ApplyUpdateV1(doc, deleteSetUpdate(sets), &writeOrigin{written: []byte{1}}))
	require.Len(t, doc.store.pendingDs.clients, parkedClients)
	merged, err := MergeUpdatesV1(updates...)
	require.NoError(t, err)
	return merged
}

// filledParkedClients is a room that parked one delete range of each of
// 1,024 clients and then took one update that gives each of them the struct
// the range deletes, so every range applies in the one transaction.
func filledParkedClients(t *testing.T) *Doc {
	t.Helper()
	doc := servedRoom()
	require.NoError(t, ApplyUpdateV1(doc, parkOneRangeEach(t, doc, 0), &writeOrigin{written: []byte{1}}))
	require.Empty(t, doc.store.pendingDs.clients)
	require.Empty(t, doc.store.pendingDsMoved)
	return doc
}

// heldOver is how many bytes more the heap holds before replace runs than
// after it; replace swaps a container of doc for one made at its size.
func heldOver(doc *Doc, replace func()) int64 {
	before := heapAfterGC()
	replace()
	after := heapAfterGC()
	runtime.KeepAlive(doc)
	return int64(before) - int64(after)
}

// pendingDsClientsOver is how many bytes more doc's map of pending-delete
// clients holds than a map made at its size with the same entries.
func pendingDsClientsOver(doc *Doc) int64 {
	return heldOver(doc, func() {
		fresh := make(map[ClientID][]DeleteRange, len(doc.store.pendingDs.clients))
		for client, ranges := range doc.store.pendingDs.clients {
			fresh[client] = ranges
		}
		doc.store.pendingDs.clients = fresh
	})
}

func TestUnit_PendingDeletes_TheMapOfClientsKeepsNoSlotsOnceItEmpties(t *testing.T) {
	doc := filledParkedClients(t)
	kept := pendingDsClientsOver(doc)
	t.Logf("the emptied map of pending-delete clients keeps %d bytes more than a fresh one", kept)
	require.LessOrEqual(t, kept, int64(spareClientsBound), "the pending deletes keep the map slots of the most clients they held")
}

func TestUnit_PendingDeletes_TheClientsGivenAStructKeepNoSlotsOnceTaken(t *testing.T) {
	doc := filledParkedClients(t)
	kept := heldOver(doc, func() { doc.store.pendingDsMoved = nil })
	t.Logf("the emptied set of clients given a struct keeps %d bytes more than none", kept)
	require.LessOrEqual(t, kept, int64(1<<10), "the set of clients given a struct keeps the map slots of its peak")
}

// refuseDrains is an apply check that refuses each apply's second check, the
// one of its drain, while refusing is set.
func refuseDrains(refusing *bool) func(any, *ApplyView) error {
	checks := 0
	return func(any, *ApplyView) error {
		if checks++; *refusing && checks%2 == 0 {
			return errors.New("the drain is refused")
		}
		return nil
	}
}

// A drain taken back gives the pending deletes back the map it found, so it
// gives back too the most clients that map has held: the drain's retry had
// replaced the map with one made at its size, and the next retry that empties
// the map it gave back would otherwise keep that map's slots.
func TestUnit_PendingDeletes_TheMapOfClientsKeepsNoSlotsAfterADrainTakenBack(t *testing.T) {
	refusing := false
	doc := New(WithClientID(1), WithMaxPendingItems(4096), WithMaxPendingDeleteRanges(4096), WithApplyCheck(refuseDrains(&refusing)))
	sender := &writeOrigin{written: []byte{1}}
	merged := parkOneRangeEach(t, doc, 1)
	refusing = true
	require.ErrorIs(t, ApplyUpdateV1(doc, merged, sender), ErrApplyCheckDrain)
	refusing = false
	require.Len(t, doc.store.pendingDs.clients, parkedClients, "the drain taken back gives every parked range back")
	w := New(WithClientID(5000))
	text := w.GetText("t")
	w.Transact(func(txn *Transaction) { text.Insert(txn, 0, "y", nil) })
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(w, nil), sender))
	require.Len(t, doc.store.pendingDs.clients, 1)
	kept := pendingDsClientsOver(doc)
	t.Logf("the map of pending-delete clients, cut from %d to 1 after a drain taken back, keeps %d bytes more than a fresh one", parkedClients, kept)
	require.LessOrEqual(t, kept, int64(spareClientsBound), "the map given back keeps the slots of its peak")
}

// A drain taken back on a document that discards refused drains keeps the
// pending deletes of the clients the fill did not reach in a map made for
// every client the drain found.
func TestUnit_DiscardRefusedDrain_TheMapOfPendingDeleteClientsKeepsNoSlotsAfterADiscard(t *testing.T) {
	doc := servedRoom()
	refusing := false
	doc.SetApplyCheck(refuseDrains(&refusing))
	merged := parkOneRangeEach(t, doc, 1)
	refusing = true
	var discarded *DrainDiscardedError
	require.ErrorAs(t, ApplyUpdateV1(doc, merged, &writeOrigin{written: []byte{1}}), &discarded)
	require.Equal(t, parkedClients-1, discarded.Ranges)
	require.Len(t, doc.store.pendingDs.clients, 1)
	kept := pendingDsClientsOver(doc)
	t.Logf("the map of pending-delete clients a discard left with 1 of %d keeps %d bytes more than a fresh one", parkedClients, kept)
	require.LessOrEqual(t, kept, int64(spareClientsBound), "the discard keeps a map made for every client the drain found")
}

// coveredQueue is a document whose queue parked 4,096 structs of client 9 at
// even clocks, the clocks between missing, and then one struct that covers
// them all, so a merge drops all but that one.
func coveredQueue(t *testing.T, parked int) *Doc {
	t.Helper()
	doc := New(WithClientID(1), WithMaxPendingItems(parked+1), WithApplyBudget(unboundedBudget))
	doc.SetDiscardRefusedDrain(true)
	sender := &writeOrigin{written: []byte{1}}
	require.NoError(t, ApplyUpdateV1(doc, structsUpdate(gappedStrings(parked)...), sender))
	cover := &Item{ID: ID{Client: 9, Clock: 1}, Origin: &ID{Client: 9, Clock: 0}, Content: NewContentDeleted(2*uint64(parked) + 1)}
	require.NoError(t, ApplyUpdateV1(doc, structsUpdate(cover), sender))
	return doc
}

// The pending queue compacts its list of structs in place once a merge has
// dropped as many as it holds (pendingUpdate.drop): the queue parks 4,096
// structs and a covering struct drops all but one.
func TestUnit_PendingQueue_TheListOfStructsKeepsNoSlotsForItsPeak(t *testing.T) {
	const parked = 4096
	doc := coveredQueue(t, parked)
	sender := &writeOrigin{written: []byte{1}}
	// A few more covered structs, so a later merge's drop compacts the list
	// on a transaction that does not view it.
	for i := range 3 {
		base := uint64(2*parked + 10 + 4*i)
		gap := &Item{ID: ID{Client: 9, Clock: base + 2}, Origin: &ID{Client: 9, Clock: base + 1}, Content: NewContentString("y")}
		require.NoError(t, ApplyUpdateV1(doc, structsUpdate(gap), sender))
		over := &Item{ID: ID{Client: 9, Clock: base + 1}, Origin: &ID{Client: 9, Clock: base}, Content: NewContentDeleted(3)}
		require.NoError(t, ApplyUpdateV1(doc, structsUpdate(over), sender))
	}
	p := doc.store.pending
	require.NotNil(t, p)
	spare := (cap(p.items) - len(p.items) + p.dead) * int(unsafe.Sizeof(p.items[0]))
	t.Logf("the queue: %d structs held, items len %d cap %d dead %d, %d spare bytes", p.heldStructs(), len(p.items), cap(p.items), p.dead, spare)
	require.LessOrEqual(t, cap(p.items), 4*max(p.heldStructs(), 1)+4, "the pending queue keeps the slots of its peak")
}

// seqRoom is how many elements s has room for: its slice's capacity, or its
// treap's nodes but the empty one, live or on the free list.
func seqRoom[T any](s seq[T]) int {
	if s.tree != nil {
		return cap(s.tree.nodes) - 1
	}
	return cap(s.flat)
}

// The queue's cover holds each client's parked clocks as a list of spans,
// which a struct that bridges them merges into one in place
// (pendingCover.insertSpan); past 256 spans the list is a treap, whose cut
// nodes wait on its free list.
func TestUnit_PendingQueue_TheCoverOfItsClocksKeepsNoSlotsForItsPeak(t *testing.T) {
	doc := coveredQueue(t, 4096)
	p := doc.store.pending
	require.NotNil(t, p)
	require.NotNil(t, p.cover, "the merge built the queue's cover")
	spans := p.cover.spans[9]
	require.Equal(t, 1, spans.len(), "the covering struct bridged every span")
	node := int(unsafe.Sizeof(treapNode[clockSpan]{}))
	t.Logf("the cover of client 9: %d span, room for %d (a treap: %t, %d bytes a node)", spans.len(), seqRoom(spans), spans.tree != nil, node)
	require.LessOrEqual(t, seqRoom(spans), 4*spans.len(), "the cover keeps the room of the most spans it held")
}
