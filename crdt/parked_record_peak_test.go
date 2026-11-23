package crdt

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeOrigin is an origin that holds the bytes of the write it applied, as a
// server write's origin does.
type writeOrigin struct{ written []byte }

// heapAfterGC is the heap in use once two collections have run.
func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// keptOverItsSize is how many bytes more the heap holds with doc's record of
// who parked each struct as it is than with the same entries in a map made at
// their size, which replaces it.
func keptOverItsSize(doc *Doc) int64 {
	withPeak := heapAfterGC()
	rebuilt := make(map[*Item]parkedFrom, len(doc.parkedBy))
	for k, v := range doc.parkedBy {
		rebuilt[k] = v
	}
	doc.parkedBy = rebuilt
	return int64(withPeak) - int64(heapAfterGC())
}

// gappedStrings is n one-character strings of client 9, each waiting for the
// clock before it, which no update holds.
func gappedStrings(n int) []*Item {
	items := make([]*Item, 0, n)
	for i := range n {
		clock := uint64(2 + 2*i)
		items = append(items, &Item{ID: ID{Client: 9, Clock: clock}, Origin: &ID{Client: 9, Clock: clock - 1}, Content: NewContentString("x")})
	}
	return items
}

// The record of who parked each struct is a map, and a struct leaves it by a
// delete as it leaves the queue (forgetGoneParked). A Go map does not shrink
// as entries are deleted, so a document whose queue held its cap of 4,096
// structs and was cut to one by a merge kept the map's slots for 4,096
// entries for as long as one struct stayed parked (about 218 KB): it is now
// replaced by a map made at its size once it falls under a quarter of its
// peak (shrinkParkedBy).
func TestUnit_ParkedRecord_KeepsNoSlotsForItsPeak(t *testing.T) {
	const parked = 4096
	doc := New(WithClientID(1), WithMaxPendingItems(parked), WithApplyBudget(unboundedBudget))
	doc.SetDiscardRefusedDrain(true)
	sender := &writeOrigin{written: []byte{1}}
	require.NoError(t, ApplyUpdateV1(doc, structsUpdate(gappedStrings(parked)...), sender))
	require.Len(t, doc.parkedBy, parked)
	cover := &Item{ID: ID{Client: 9, Clock: 1}, Origin: &ID{Client: 9, Clock: 0}, Content: NewContentDeleted(2*parked + 1)}
	require.NoError(t, ApplyUpdateV1(doc, structsUpdate(cover), sender))
	require.Equal(t, 1, doc.PendingStats().Items)

	kept := keptOverItsSize(doc)
	runtime.KeepAlive(doc)
	runtime.KeepAlive(sender)
	t.Logf("the record of 1 struct keeps %d bytes more than one made at its size", kept)
	require.LessOrEqual(t, kept, int64(64<<10), "the record of parked structs keeps the slots of its peak")
}

// ForgetParkedOrigin deletes from the record of who parked each struct too:
// once the sender of 4,096 parked structs is forgotten, the record of the one
// struct another sender parked is held in a map made at its size.
func TestUnit_ParkedRecord_ForgettingAnOriginLeavesNoSlotsForItsStructs(t *testing.T) {
	const parked = 4096
	doc := New(WithClientID(1), WithMaxPendingItems(parked+1), WithApplyBudget(unboundedBudget))
	doc.SetDiscardRefusedDrain(true)
	left, stays := &writeOrigin{written: []byte{1}}, &writeOrigin{written: []byte{2}}
	require.NoError(t, ApplyUpdateV1(doc, structsUpdate(gappedStrings(parked)...), left))
	other := &Item{ID: ID{Client: 10, Clock: 2}, Origin: &ID{Client: 10, Clock: 1}, Content: NewContentString("y")}
	require.NoError(t, ApplyUpdateV1(doc, structsUpdate(other), stays))
	require.Len(t, doc.parkedBy, parked+1)
	doc.ForgetParkedOrigin(left)
	require.Len(t, doc.parkedBy, 1)

	kept := keptOverItsSize(doc)
	runtime.KeepAlive(doc)
	t.Logf("the record of 1 struct keeps %d bytes more than one made at its size", kept)
	require.LessOrEqual(t, kept, int64(64<<10), "the record of parked structs keeps the slots of the origin forgotten")
}

// ForgetParkedOrigin cuts the record of who parked each delete range in
// place, one set for each sender: once 1,023 of 1,024 senders are forgotten,
// the set left is held in an array made for no more than about four sets.
func TestUnit_ParkedRecord_TheRecordOfParkedDeletesKeepsNoSlotsForItsPeak(t *testing.T) {
	doc := New(WithClientID(1), WithMaxPendingDeleteRanges(4096), WithApplyBudget(unboundedBudget))
	doc.SetDiscardRefusedDrain(true)
	senders := make([]*writeOrigin, 1024)
	for k := range senders {
		senders[k] = &writeOrigin{written: []byte{1}}
		u := deleteSetUpdate(map[ClientID][]DeleteRange{9: {{Clock: uint64(2 * k), Len: 1}}})
		require.NoError(t, ApplyUpdateV1(doc, u, senders[k]))
	}
	require.Len(t, doc.parkedDsBy, len(senders))
	for _, s := range senders[1:] {
		doc.ForgetParkedOrigin(s)
	}
	require.Len(t, doc.parkedDsBy, 1)
	require.GreaterOrEqual(t, len(doc.parkedDsBy), cap(doc.parkedDsBy)/4,
		"the record of parked delete ranges holds 1 set in an array of %d", cap(doc.parkedDsBy))
}
