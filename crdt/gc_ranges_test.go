package crdt

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_GCTransaction_OverlappingRanges(t *testing.T) {
	doc := newTestDoc(1)
	defer doc.Destroy()
	txn := newTxn(doc)
	for i := 0; i < 6; i++ {
		doc.store.Append(&Item{ID: ID{Client: 1, Clock: uint64(i * 4)}, Content: NewContentString("abcd"), Deleted: i != 2})
	}
	// Includes a partial item, unsorted and overlapping ranges, and a live item.
	txn.deleteSet.clients[1] = []DeleteRange{{Clock: 19, Len: 1}, {Clock: 3, Len: 7}, {Clock: 5, Len: 1}, {Clock: 24, Len: 5}}
	txn.deleteSet.clients[2] = []DeleteRange{{Clock: 0, Len: 1}}
	gcTxnDeleteSet(doc, txn)
	for i, item := range doc.store.clients[1] {
		_, gc := item.Content.(*ContentDeleted)
		require.Equal(t, i == 0 || i == 1 || i == 4, gc, "item %d", i)
		require.Equal(t, 4, item.Content.Len())
	}
}

func BenchmarkGCTransactionSparseRanges(b *testing.B) {
	for _, size := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			doc := newTestDoc(1)
			defer doc.Destroy()
			txn := newTxn(doc)
			for i := 0; i < size; i++ {
				doc.store.Append(&Item{ID: ID{Client: 1, Clock: uint64(i)}, Content: NewContentDeleted(1), Deleted: true})
				if i%10 == 0 {
					txn.deleteSet.clients[1] = append(txn.deleteSet.clients[1], DeleteRange{Clock: uint64(i), Len: 1})
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				gcTxnDeleteSet(doc, txn)
			}
		})
	}
}
