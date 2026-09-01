package crdt

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ── Fragmented-document fixture ──────────────────────────────────────────────
//
// The pre-existing BenchmarkObservedTxn_Apply (bench_test.go) cannot see the
// cost these benchmarks target: a single client appending merges into a handful
// of ContentString items, so its observer walk is O(items) and effectively
// O(1) no matter how many characters the document holds.
//
// newFragmentedDoc builds a document that genuinely holds one item per
// character. It is O(n): an earlier version interleaved two clients and synced
// them per character, which cost two encode+apply round trips per character and
// was O(n^2) — 4k characters took 60ms, making large sizes untestable.
//
// Two independent properties keep it fragmented, and it is worth being precise
// about which does what, because a future edit could remove one while believing
// the other is doing the work:
//
//   - Each insert is its own transaction, so squashRuns — which only collapses
//     a same-client run created within ONE transaction — never sees a run to
//     collapse. Verified: 500 SEQUENTIAL end-inserts, one per transaction, still
//     produce 500 distinct items.
//   - Positions are random, so items are not contiguous in the linked list even
//     when they do share a transaction. Verified: 500 random-position inserts in
//     a SINGLE transaction also stay distinct.
//
// Note that randomness alone does not guarantee non-contiguity — two draws can
// land adjacent — so the per-transaction property is the load-bearing one; the
// random positions additionally split the seed item, which is why the item count
// is n+4 rather than n+1.
//
// The RNG is seeded so the shape is identical across runs and machines.
func newFragmentedDoc(n int) (*Doc, *YText) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "seed", nil) })
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		pos := rng.Intn(txt.Len())
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, pos, "x", nil) })
	}
	return doc, txt
}

// TestNewFragmentedDocIsFragmented guards the fixture. If a future change lets
// these inserts merge, the benchmarks below would silently start measuring a
// short item list while still reporting a large n — the exact failure that
// makes BenchmarkObservedTxn_Apply blind.
//
// It counts the inserted single-character items exactly rather than checking a
// total against a threshold. A total-count check is too loose to be a guard: the
// random inserts split the 4-character seed into as many as four items, so a
// document that had quietly merged a handful of inserts could still clear a
// "total >= n" bar and report success while measuring the wrong shape.
func TestNewFragmentedDocIsFragmented(t *testing.T) {
	const n = 2000
	_, txt := newFragmentedDoc(n)

	singles := 0
	for it := txt.start; it != nil; it = it.Right {
		cs, ok := it.Content.(*ContentString)
		if !ok {
			continue
		}
		if cs.Str == "x" {
			singles++
			continue
		}
		// Anything else must be a piece of the seed. An "x" that has merged
		// with a neighbour shows up here as a longer run containing one.
		if strings.Contains(cs.Str, "x") {
			t.Fatalf("fixture merged: found item %q containing an inserted character; "+
				"the benchmarks would measure a shorter item list than the n they report", cs.Str)
		}
	}
	if singles != n {
		t.Fatalf("fixture merged: %d distinct single-character items, want exactly %d", singles, n)
	}
}

// ── Observed-transaction cost ────────────────────────────────────────────────

// benchObservedFragmented measures one single-character insert on a document of
// roughly n items, with and without an observer attached. The difference
// between the two is the cost of building the observer's delta.
//
// The rebuild guard is load-bearing. Each iteration inserts, so the document
// grows as the benchmark runs; Go chooses b.N by timing progressively larger
// runs, so without the guard the reported ns/op depends on b.N rather than on
// n, and a size-labelled result would not describe the size it names. The
// document is rebuilt (with the timer stopped) once it has grown 2% past n.
func benchObservedFragmented(b *testing.B, n int, withObserver bool) {
	doc, txt := newFragmentedDoc(n)
	var unsub func()
	if withObserver {
		unsub = txt.Observe(func(YTextEvent) {})
	}
	defer func() {
		if unsub != nil {
			unsub()
		}
	}()

	maxGrowth := n / 50
	if maxGrowth < 1 {
		maxGrowth = 1
	}
	grown := 0

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if grown >= maxGrowth {
			b.StopTimer()
			if unsub != nil {
				unsub()
				unsub = nil
			}
			doc, txt = newFragmentedDoc(n)
			if withObserver {
				unsub = txt.Observe(func(YTextEvent) {})
			}
			grown = 0
			b.StartTimer()
		}
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "x", nil) })
		grown++
	}
}

func BenchmarkObservedTxn_Fragmented(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d/observed", n), func(b *testing.B) { benchObservedFragmented(b, n, true) })
		b.Run(fmt.Sprintf("n=%d/baseline", n), func(b *testing.B) { benchObservedFragmented(b, n, false) })
	}
}

// ── Per-insert allocations ───────────────────────────────────────────────────

// BenchmarkItemAlloc_SingleCharInsert reports allocs/op and B/op for one
// single-character insert. Item.Origin, Item.OriginRight and Item.parentID are
// *ID, and the allocations behind those pointers are what #188's item-origin
// bullet is about.
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

// ── Delete-set lookup ────────────────────────────────────────────────────────

// BenchmarkDeleteSet_IsDeleted measures IsDeleted against the number of delete
// ranges held by one client.
//
// The small counts are the important ones, and an earlier version of this
// benchmark omitted them. A transaction's delete set holds one range per
// distinct deleted region, so ordinary editing produces 0 or 1 — a pure insert
// produces an EMPTY set. Measuring only 1/10/100/1000 hides what happens in the
// range where real workloads sit, and computeDelta calls IsDeleted once per
// pre-existing item, so a fraction of a nanosecond here is multiplied by the
// document's item count on every observed transaction.
func BenchmarkDeleteSet_IsDeleted(b *testing.B) {
	for _, ranges := range []int{0, 1, 2, 4, 8, 16, 100, 1_000} {
		b.Run(fmt.Sprintf("ranges=%d", ranges), func(b *testing.B) {
			ds := newDeleteSet()
			for i := 0; i < ranges; i++ {
				ds.add(ID{Client: 1, Clock: uint64(i * 4)}, 2)
			}
			// Probe the LAST range, the worst case for a linear scan. With no
			// ranges at all, probe a clock that cannot match.
			probe := ID{Client: 1, Clock: 1}
			if ranges > 0 {
				probe = ID{Client: 1, Clock: uint64(ranges*4 - 3)}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = ds.IsDeleted(probe)
			}
		})
	}
}
