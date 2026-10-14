//go:build benchheavy

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

// Varying dependencies exercise metadata bounds at a pending cap of 16.
// Fixture allocation is outside the timer.
func BenchmarkPendingUpdateDiverseDependencies(b *testing.B) {
	for _, version := range []int{1, 2} {
		for _, shape := range []string{"missing", "lengths", "unrelated", "cycle", "groups", "groups-unrelated"} {
			for _, n := range []int{1000, 20000, 500000} {
				b.Run(fmt.Sprintf("V%d/%s/n=%d", version, shape, n), func(b *testing.B) {
					data := pendingDiverseUpdate(version, n, shape)
					apply := ApplyUpdateV1
					if version == 2 {
						apply = ApplyUpdateV2
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						doc := New(WithMaxPendingItems(16))
						if err := apply(doc, data, nil); err == nil {
							b.Fatal("incomplete update accepted")
						}
						if doc.PendingStats().Items != 0 {
							b.Fatal("rejected update parked items")
						}
						doc.Destroy()
					}
				})
			}
		}
	}
}
