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
