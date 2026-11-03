package crdt

import (
	"fmt"
	"testing"
)

// separateItems returns a doc whose client 1 holds a deleted run of 8 and then
// n characters typed one by one at index 0: n+1 items.
func separateItems(n int) *Doc {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcdefgh", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 8) })
	for range n {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "a", nil) })
	}
	return doc
}

// BenchmarkApplyUpdateV1_RedeleteInsideDeletedRun applies a delete set that
// deletes again three clocks inside client 1's deleted run.
func BenchmarkApplyUpdateV1_RedeleteInsideDeletedRun(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("items=%d", n), func(b *testing.B) {
			doc := separateItems(n)
			update := deleteSetUpdate(1, 2, 3)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := ApplyUpdateV1(doc, update, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
