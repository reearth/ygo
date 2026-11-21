package crdt

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tests for what a document with a budget or a roots list keeps of the apply
// that parked each struct (parkedBy): a struct that leaves the pending queue,
// for any reason, leaves it, so it never holds a struct, or a struct's
// content, the queue does not hold.

// structsUpdate encodes items, which no document holds, as one V1 update.
func structsUpdate(items ...*Item) []byte {
	perClient := map[ClientID][]*Item{}
	for _, it := range items {
		perClient[it.ID.Client] = append(perClient[it.ID.Client], it)
	}
	return encodeStructStoreV1(perClient, newDeleteSet(), nil)
}

// unboundedBudget is a budget that bounds nothing, so the document keeps who
// parked each struct and refuses nothing for it.
func unboundedBudget(any) ApplyBudget { return ApplyBudget{} }

// requireRecordWithinQueue requires what doc keeps of who parked each struct
// to hold no struct the pending queue does not hold, and so no more structs
// than the queue holds.
func requireRecordWithinQueue(t *testing.T, doc *Doc, step string) {
	t.Helper()
	held := make(map[*Item]struct{})
	if p := doc.store.pending; p != nil {
		for _, it := range p.heldItems() {
			held[it] = struct{}{}
		}
	}
	require.LessOrEqual(t, len(doc.parkedBy), doc.PendingStats().Items,
		"after %s the record of who parked each struct holds more structs than the queue", step)
	for it := range doc.parkedBy {
		_, ok := held[it]
		require.True(t, ok, "after %s the record holds client %d's struct at clock %d, which the queue does not hold",
			step, it.ID.Client, it.ID.Clock)
	}
}

// coverParkedStrings runs, n times, two frames: the first parks a string of
// size bytes behind a gap of client 9, and the second parks a deleted struct,
// one clock earlier, that covers it, so the merge drops the string. Each pass
// leaves the queue one struct longer and the record told of two. parked, when
// not nil, gets each string's struct once it parks, and check runs after each
// frame.
func coverParkedStrings(t *testing.T, doc *Doc, n, size int, parked func(*Item), check func(step string)) {
	t.Helper()
	clock := uint64(2)
	for i := range n {
		str := &Item{ID: ID{Client: 9, Clock: clock}, Origin: &ID{Client: 9, Clock: clock - 1},
			Content: NewContentString(strings.Repeat(string(rune('a'+i%26)), size))}
		require.NoError(t, ApplyUpdateV1(doc, structsUpdate(str), "a"), "pass %d's string", i)
		if parked != nil {
			held := doc.store.pending.heldItems()
			parked(held[len(held)-1])
		}
		check(fmt.Sprintf("pass %d's string", i))
		cover := &Item{ID: ID{Client: 9, Clock: clock - 1}, Origin: &ID{Client: 9, Clock: clock - 2},
			Content: NewContentDeleted(uint64(size) + 1)}
		require.NoError(t, ApplyUpdateV1(doc, structsUpdate(cover), "a"), "pass %d's cover", i)
		check(fmt.Sprintf("pass %d's cover", i))
		require.Equal(t, i+1, doc.PendingStats().Items, "after pass %d the covers alone are parked", i)
		clock += uint64(size) + 3
	}
}

// typedTwice is client's "a" and then its "b" after it, each as the V1
// update a document holding what came before takes: the second waits on the
// first.
func typedTwice(client ClientID) (first, second []byte) {
	w := New(WithClientID(client))
	text := w.GetText("t")
	w.Transact(func(txn *Transaction) { text.Insert(txn, 0, "a", nil) })
	first = EncodeStateAsUpdateV1(w, nil)
	sv := w.StateVector()
	w.Transact(func(txn *Transaction) { text.Insert(txn, 1, "b", nil) })
	return first, EncodeStateAsUpdateV1(w, sv)
}

// A struct that leaves the pending queue leaves the record of who parked it,
// whether a merge drops it, a drain lands it or the apply that parked it is
// rolled back. The record used to keep every one until nothing was parked: a
// merge that drops a parked string while the struct that covers it parks
// leaves the queue one struct longer and the record two, each dropped string
// reachable from it.
func TestUnit_ParkedRecord_HoldsOnlyWhatTheQueueHolds(t *testing.T) {
	t.Run("dropped by a merge", func(t *testing.T) {
		doc := New(WithClientID(1), WithApplyBudget(unboundedBudget))
		coverParkedStrings(t, doc, 40, 64, nil, func(step string) { requireRecordWithinQueue(t, doc, step) })
	})

	t.Run("landed by a drain", func(t *testing.T) {
		doc := New(WithClientID(1), WithApplyBudget(unboundedBudget))
		fill, lands := typedTwice(9)
		_, stays := typedTwice(8)
		require.NoError(t, ApplyUpdateV1(doc, lands, "a"))
		require.NoError(t, ApplyUpdateV1(doc, stays, "b"))
		require.Len(t, doc.parkedBy, 2)
		require.NoError(t, ApplyUpdateV1(doc, fill, "c"))
		require.Equal(t, 1, doc.PendingStats().Items, "the fill lands client 9's struct, and client 8's stays parked")
		requireRecordWithinQueue(t, doc, "the fill")
	})

	t.Run("rolled back with the apply that parked it", func(t *testing.T) {
		doc := New(WithClientID(1), WithWholeOrNothingApplies(true),
			WithApplyBudget(func(any) ApplyBudget { return ApplyBudget{Structs: 1} }))
		_, stays := typedTwice(8)
		require.NoError(t, ApplyUpdateV1(doc, stays, "b"))
		two := structsUpdate(
			&Item{ID: ID{Client: 9, Clock: 1}, Origin: &ID{Client: 9, Clock: 0}, Content: NewContentString("x")},
			&Item{ID: ID{Client: 10, Clock: 1}, Origin: &ID{Client: 10, Clock: 0}, Content: NewContentString("y")},
		)
		require.ErrorIs(t, ApplyUpdateV1(doc, two, "a"), ErrApplyBudget, "two parked structs pass a budget of one")
		require.Equal(t, 1, doc.PendingStats().Items, "the refused apply parks nothing")
		requireRecordWithinQueue(t, doc, "the refused apply")
	})
}
