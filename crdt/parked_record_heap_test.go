//go:build go1.24

package crdt

import (
	"runtime"
	"testing"
	"unsafe"
	"weak"
)

// The test in this file needs the weak package, which Go 1.24 added; the
// module still builds with Go 1.23, which leaves it out.

// A string a merge drops from the pending queue is freed once the frame that
// dropped it is applied: the record of who parked each struct lets it go as it
// leaves the queue. It used to keep every one until nothing was parked.
func TestUnit_ParkedRecord_AStringAMergeDroppedIsFreed(t *testing.T) {
	doc := New(WithClientID(1), WithApplyBudget(unboundedBudget))
	var dropped []weak.Pointer[byte]
	coverParkedStrings(t, doc, 16, 256<<10, func(it *Item) {
		dropped = append(dropped, weak.Make(unsafe.StringData(it.Content.(*ContentString).Str)))
	}, func(string) {})
	runtime.GC()
	for i, w := range dropped {
		if w.Value() != nil {
			t.Fatalf("pass %d's string, which the merge dropped, is still reachable after a collection", i)
		}
	}
	runtime.KeepAlive(doc)
}
