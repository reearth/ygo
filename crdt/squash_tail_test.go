package crdt

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// Squashing two characters typed mid-text leaves every earlier item of their
// client in place, and the receiver ends with the writer's text.
func TestUnit_SquashRuns_KeepsTheItemsBeforeTheRun(t *testing.T) {
	t.Parallel()
	writer := newTestDoc(1)
	wt := writer.GetText("t")
	for range 50 {
		writer.Transact(func(txn *Transaction) { wt.Insert(txn, 0, "a", nil) })
	}
	doc := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(writer, nil), nil))
	before := slices.Clone(doc.store.clients[1])

	sv := writer.StateVector()
	writer.Transact(func(txn *Transaction) { wt.Insert(txn, 25, "k", nil) })
	writer.Transact(func(txn *Transaction) { wt.Insert(txn, 26, "j", nil) })
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(writer, sv), nil))

	items := doc.store.clients[1]
	require.Len(t, items, len(before)+1, "the two characters squash into one item")
	for i, it := range before {
		require.Same(t, it, items[i], "item %d", i)
	}
	require.Equal(t, "kj", items[len(before)].Content.(*ContentString).Str)
	require.Equal(t, wt.ToString(), doc.GetText("t").ToString())
}

// dropSquashed drops exactly the squashed items wherever the first of them
// sits, and clears the slots it frees.
func TestUnit_DropSquashed_Boundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		removed []int // indexes into the store's five items
	}{
		{"the first item", []int{0}},
		{"the first two items", []int{0, 1}},
		{"one item in the middle", []int{2}},
		{"two items apart", []int{1, 3}},
		{"the last item", []int{4}},
		{"every item", []int{0, 1, 2, 3, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := make([]*Item, 5)
			for i := range store {
				store[i] = &Item{ID: ID{Client: 1, Clock: uint64(i)}, Content: NewContentString("a")}
			}
			all := slices.Clone(store)
			var removed, want []*Item
			for i, it := range all {
				if slices.Contains(tc.removed, i) {
					removed = append(removed, it)
				} else {
					want = append(want, it)
				}
			}
			got := dropSquashed(store, removed)
			require.Equal(t, len(want), len(got))
			for i := range want {
				require.Same(t, want[i], got[i], "item %d", i)
			}
			for i := len(got); i < len(store); i++ {
				require.Nil(t, store[i], "freed slot %d", i)
			}
		})
	}
}
