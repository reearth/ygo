//go:build benchheavy

package crdt

import (
	"fmt"
	"testing"
)

func BenchmarkPendingCursorBomb(b *testing.B) {
	for _, version := range []int{1, 2} {
		update := pendingCursorBomb(version, 500000)
		apply := ApplyUpdateV1
		if version == 2 {
			apply = ApplyUpdateV2
		}
		b.Run(fmt.Sprintf("V%d", version), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				d := New(WithMaxPendingItems(16))
				if err := apply(d, update, nil); err == nil {
					b.Fatal("incomplete update accepted")
				}
				if d.PendingStats().Items != 0 {
					b.Fatal("rejection parked items")
				}
				d.Destroy()
			}
		})
	}
}
