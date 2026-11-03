package crdt

import (
	"fmt"
	"testing"
)

// BenchmarkGCTransactionNestedType measures the transaction that deletes a map
// entry holding an array of n map rows, collection included, and reports the
// size of the document's V1 state after it. Building the document is excluded.
func BenchmarkGCTransactionNestedType(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			var doc *Doc
			for range b.N {
				b.StopTimer()
				var m *YMap
				doc, m = nestedRowsDoc(n)
				b.StartTimer()
				doc.Transact(func(txn *Transaction) { m.Delete(txn, "rows") })
			}
			b.StopTimer()
			b.ReportMetric(float64(len(EncodeStateAsUpdateV1(doc, nil))), "state-bytes")
		})
	}
}

// BenchmarkRunGCNestedType measures RunGC after the delete of a map entry
// holding an array of n map rows, which the transaction GC skipped because an
// UndoManager is attached, and reports the size of the document's V1 state
// after it. Building the document and the delete are excluded.
func BenchmarkRunGCNestedType(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			var doc *Doc
			for range b.N {
				b.StopTimer()
				var m *YMap
				doc, m = nestedRowsDoc(n)
				NewUndoManager(doc, []SharedType{m})
				doc.Transact(func(txn *Transaction) { m.Delete(txn, "rows") })
				b.StartTimer()
				RunGC(doc)
			}
			b.StopTimer()
			b.ReportMetric(float64(len(EncodeStateAsUpdateV1(doc, nil))), "state-bytes")
		})
	}
}

// nestedRowsDoc returns a document whose map "m" holds, under "rows", an array
// of n maps with one entry each.
func nestedRowsDoc(n int) (*Doc, *YMap) {
	doc := New(WithClientID(1))
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		rows := NewArrayPrelim()
		m.Set(txn, "rows", rows)
		for i := range n {
			row := NewMapPrelim()
			rows.PushType(txn, row)
			row.Set(txn, "v", i)
		}
	})
	return doc, m
}
