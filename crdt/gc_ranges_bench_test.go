package crdt

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkGCTransactionStringRanges measures collection of string content,
// including the cost of visiting each item and allocating its tombstone.
// Fixture construction and restoring string content between runs are excluded.
func BenchmarkGCTransactionStringRanges(b *testing.B) {
	for _, tc := range []struct {
		name       string
		rangeItems int
	}{
		{"Sparse", 1},
		{"Wide", 8},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for _, size := range []int{1000, 10000, 50000} {
				b.Run(fmt.Sprint(size), func(b *testing.B) {
					doc := newTestDoc(1)
					defer doc.Destroy()
					txn := newTxn(doc)
					// 384 UTF-16 units per item, including supplementary characters.
					// GC only replaces Content, so sharing this immutable string is safe.
					content := NewContentString(strings.Repeat("a😀", 128))
					length := uint64(content.Len())
					for i := 0; i < size; i++ {
						clock := uint64(i) * length
						doc.store.Append(&Item{ID: ID{Client: 1, Clock: clock}, Content: content, Deleted: true})
						if i%10 == 0 {
							txn.deleteSet.clients[1] = append(txn.deleteSet.clients[1], DeleteRange{
								Clock: clock + 1,
								Len:   uint64(tc.rangeItems)*length - 2,
							})
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						for _, item := range doc.store.clients[1] {
							item.Content = content
						}
						b.StartTimer()
						gcTxnDeleteSet(doc, txn)
					}
					b.StopTimer()
				})
			}
		})
	}
}
