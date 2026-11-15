package crdt

import (
	"fmt"
	"testing"
)

// buildHalfDeletedTextDoc returns a doc whose "text" YText holds n items that
// alternate between live and deleted, so each deleted item is its own range.
func buildHalfDeletedTextDoc(n int) *Doc {
	doc := newTestDoc(1)
	txt := doc.GetText("text")
	doc.Transact(func(txn *Transaction) {
		for range n / 2 {
			txt.Insert(txn, 0, "ab", nil)
			txt.Delete(txn, 1, 1)
		}
	})
	return doc
}

// BenchmarkBuildDeleteSet builds the delete set of a half-deleted text, as
// every full-state encode and snapshot does.
func BenchmarkBuildDeleteSet(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		doc := buildHalfDeletedTextDoc(n)
		if got := len(buildDeleteSet(doc.store).clients[1]); got != n/2 {
			b.Fatalf("fixture has %d deleted ranges, want %d", got, n/2)
		}
		b.Run(fmt.Sprintf("items=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = buildDeleteSet(doc.store)
			}
		})
	}
}

// BenchmarkEncodeHalfDeletedText measures the full-state encoders and
// CaptureSnapshot on a half-deleted text, where the delete set is large.
func BenchmarkEncodeHalfDeletedText(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		doc := buildHalfDeletedTextDoc(n)
		b.Run(fmt.Sprintf("V1/items=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = EncodeStateAsUpdateV1(doc, nil)
			}
		})
		b.Run(fmt.Sprintf("V2/items=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = EncodeStateAsUpdateV2(doc, nil)
			}
		})
		b.Run(fmt.Sprintf("CaptureSnapshot/items=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = CaptureSnapshot(doc)
			}
		})
	}
}
