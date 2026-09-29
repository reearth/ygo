package crdt

import (
	"fmt"
	"testing"
)

func BenchmarkPendingUpdateIncomplete(b *testing.B) {
	for _, version := range []int{1, 2} {
		b.Run(fmt.Sprintf("V%d", version), func(b *testing.B) {
			update, _ := pendingBudgetUpdate(version, 500_000, "missing")
			apply := ApplyUpdateV1
			if version == 2 {
				apply = ApplyUpdateV2
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				doc := New()
				if err := apply(doc, update, nil); err == nil {
					b.Fatal("incomplete update was accepted")
				}
				doc.Destroy()
			}
		})
	}
}
