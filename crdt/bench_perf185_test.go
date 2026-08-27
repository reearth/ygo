package crdt

import (
	"fmt"
	"testing"
)

// buildFragmented returns a doc whose YText "t" holds n single-character items
// that CANNOT merge: two clients alternate, so no two adjacent items share a
// client. This is the shape a real multi-peer document takes, and the shape the
// existing BenchmarkObservedTxn_Apply fails to produce.
func buildFragmented(n int) (*Doc, *YText) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	other := newTestDoc(2)
	otherTxt := other.GetText("t")
	for i := 0; i < n/2; i++ {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "a", nil) })
		other.Transact(func(txn *Transaction) { otherTxt.Insert(txn, otherTxt.Len(), "b", nil) })
		if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(other, doc.StateVector()), nil); err != nil {
			panic(err)
		}
		if err := ApplyUpdateV1(other, EncodeStateAsUpdateV1(doc, other.StateVector()), nil); err != nil {
			panic(err)
		}
	}
	return doc, txt
}

func benchObservedFragmented(b *testing.B, n int, withObserver bool) {
	doc, txt := buildFragmented(n)
	if withObserver {
		unsub := txt.Observe(func(YTextEvent) {})
		defer unsub()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "x", nil) })
	}
}

// The C1 gate (spec §7) compares the baseline-subtracted cost at 100k against
// 1k. Both the observed and baseline variants are required: the difference
// between them is the delta-computation cost, which is the quantity #185 is
// about.
func BenchmarkObservedTxn_Fragmented(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d/observed", n), func(b *testing.B) { benchObservedFragmented(b, n, true) })
		b.Run(fmt.Sprintf("n=%d/baseline", n), func(b *testing.B) { benchObservedFragmented(b, n, false) })
	}
}

// BenchmarkItemAlloc_SingleCharInsert is E5's before/after measurement: the
// allocs/op and B/op attributable to Origin, OriginRight and parentID being
// *ID. Task 6 quotes the delta on #188.
func BenchmarkItemAlloc_SingleCharInsert(b *testing.B) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "seed", nil) })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "x", nil) })
	}
}

// BenchmarkDeleteSet_IsDeleted isolates E1's win at range counts that span the
// crossover from linear-is-fine to binary-search-wins.
func BenchmarkDeleteSet_IsDeleted(b *testing.B) {
	for _, ranges := range []int{1, 10, 100, 1_000} {
		b.Run(fmt.Sprintf("ranges=%d", ranges), func(b *testing.B) {
			ds := newDeleteSet()
			for i := 0; i < ranges; i++ {
				ds.add(ID{Client: 1, Clock: uint64(i * 4)}, 2)
			}
			probe := ID{Client: 1, Clock: uint64(ranges*4 - 3)} // worst case: last range
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = ds.IsDeleted(probe)
			}
		})
	}
}

func TestBuildFragmentedActuallyFragments(t *testing.T) {
	doc, txt := buildFragmented(1000)
	n := 0
	for item := txt.start; item != nil; item = item.Right {
		n++
	}
	// A merged doc would collapse to a handful of items. Require at least half
	// the inserted characters to remain as distinct items.
	if n < 500 {
		t.Fatalf("expected a fragmented doc (>=500 items), got %d — the benchmark would be measuring the wrong shape", n)
	}
	_ = doc
}
