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

// squashedPairs returns a receiver loaded with state, which holds client 1's
// items, and k updates of client 1 that each carry two characters typed in
// two transactions, which the receiver squashes into one item.
func squashedPairs(b *testing.B, state []byte, k int) (*Doc, [][]byte) {
	b.Helper()
	doc, writer := New(WithClientID(2)), New(WithClientID(1))
	for _, d := range []*Doc{doc, writer} {
		if err := ApplyUpdateV1(d, state, nil); err != nil {
			b.Fatal(err)
		}
	}
	txt := writer.GetText("t")
	updates := make([][]byte, k)
	for i := range updates {
		sv := writer.StateVector()
		writer.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "k", nil) })
		writer.Transact(func(txn *Transaction) { txt.Insert(txn, 1, "j", nil) })
		items := writer.store.clients[1]
		updates[i] = encodeStructStoreV1(map[ClientID][]*Item{1: items[len(items)-2:]}, newDeleteSet(), sv, writer.store)
	}
	return doc, updates
}

// BenchmarkApplyUpdateV1_SquashedPair applies squashedPairs updates to a fresh
// receiver every n/10 updates, so client 1 holds between n+1 and 1.1n+1 items.
func BenchmarkApplyUpdateV1_SquashedPair(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("items=%d", n), func(b *testing.B) {
			state := EncodeStateAsUpdateV1(separateItems(n), nil)
			b.ReportAllocs()
			b.ResetTimer()
			for done := 0; done < b.N; {
				b.StopTimer()
				doc, updates := squashedPairs(b, state, min(n/10, b.N-done))
				before := len(doc.store.clients[1])
				b.StartTimer()
				for _, u := range updates {
					if err := ApplyUpdateV1(doc, u, nil); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				if got := len(doc.store.clients[1]) - before; got != len(updates) {
					b.Fatalf("%d updates added %d items, want one squashed item each", len(updates), got)
				}
				done += len(updates)
				b.StartTimer()
			}
		})
	}
}
