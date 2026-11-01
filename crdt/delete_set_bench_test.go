package crdt

import (
	"fmt"
	"testing"
)

// BenchmarkDeleteSet_SortAndCompact sorts and merges one client's delete
// ranges, given in descending clock order.
func BenchmarkDeleteSet_SortAndCompact(b *testing.B) {
	for _, n := range []int{1, 16, 1_000} {
		b.Run(fmt.Sprintf("ranges=%d", n), func(b *testing.B) {
			descending := make([]DeleteRange, n)
			for i := range descending {
				descending[i] = DeleteRange{Clock: uint64(2 * (n - i)), Len: 1}
			}
			ranges := make([]DeleteRange, n)
			ds := newDeleteSet()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				copy(ranges, descending)
				ds.clients[1] = ranges
				ds.sortAndCompact(1)
			}
		})
	}
}

// BenchmarkEncodeStateAsUpdate_DeleteSet encodes a document in which each
// client wrote 100 characters and deleted every other one.
func BenchmarkEncodeStateAsUpdate_DeleteSet(b *testing.B) {
	for _, clients := range []int{1, 4} {
		doc := newTestDoc(1)
		for c := range clients {
			peer := newTestDoc(uint64(c + 2))
			txt := peer.GetText("t")
			for range 100 {
				peer.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "a", nil) })
			}
			for i := range 50 {
				peer.Transact(func(txn *Transaction) { txt.Delete(txn, i, 1) })
			}
			if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(peer, nil), nil); err != nil {
				b.Fatal(err)
			}
		}
		for _, v := range []struct {
			name   string
			encode func(*Doc, StateVector) []byte
		}{{"V1", EncodeStateAsUpdateV1}, {"V2", EncodeStateAsUpdateV2}} {
			b.Run(fmt.Sprintf("%s/clients=%d", v.name, clients), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					v.encode(doc, nil)
				}
			})
		}
	}
}
