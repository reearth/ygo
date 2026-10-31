package crdt

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkOnUpdate_KeystrokeAfterDeletions types one character at the end of
// a text whose history holds the given number of separate deletions, with an
// OnUpdate observer registered, and reports the size of the update it gets.
func BenchmarkOnUpdate_KeystrokeAfterDeletions(b *testing.B) {
	for _, deletions := range []int{0, 1_000, 10_000} {
		b.Run(fmt.Sprintf("deletions=%d", deletions), func(b *testing.B) {
			doc := newTestDoc(1)
			txt := doc.GetText("t")
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, strings.Repeat("ab", deletions), nil) })
			for i := deletions - 1; i >= 0; i-- {
				doc.Transact(func(txn *Transaction) { txt.Delete(txn, 2*i+1, 1) })
			}
			size := 0
			unsub := doc.OnUpdate(func(update []byte, _ any) { size = len(update) })
			defer unsub()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "x", nil) })
			}
			b.ReportMetric(float64(size), "update-bytes")
		})
	}
}
