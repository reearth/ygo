//go:build benchheavy

package crdt

import (
	"math/rand"
	"testing"
)

// nHeavy is the document size used by the heavy-tier 100k random-access
// benchmarks below. These are gated behind the benchheavy build tag because
// building a 100k-element document is too slow for the default PR test gate.
const nHeavy = 100_000

// buildHeavyTextDoc builds a doc whose YText "t" contains n single-character
// inserts, appended one at a time (mirrors buildTextDoc in bench_test.go but
// lives here so this file has no dependency on non-benchheavy helpers).
func buildHeavyTextDoc(n int) (*Doc, *YText) {
	doc := New(WithClientID(ClientID(1)))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		for i := 0; i < n; i++ {
			txt.Insert(txn, txt.Len(), "a", nil)
		}
	})
	return doc, txt
}

// buildHeavyArrayDoc builds a doc whose YArray "a" contains n scalar
// elements, appended one at a time.
func buildHeavyArrayDoc(n int) (*Doc, *YArray) {
	doc := New(WithClientID(ClientID(1)))
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		for i := 0; i < n; i++ {
			arr.Insert(txn, arr.Len(), []any{i})
		}
	})
	return doc, arr
}

// BenchmarkYArray_RandomGet_100k measures the cost of a single positional
// Get at a uniformly random index into a 100k-element YArray. This exercises
// the move-aware search-marker path (see v1.39.0) that makes positional
// access O(1) rather than O(n).
func BenchmarkYArray_RandomGet_100k(b *testing.B) {
	b.ReportAllocs()

	_, arr := buildHeavyArrayDoc(nHeavy)
	r := rand.New(rand.NewSource(42))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = arr.Get(r.Intn(nHeavy))
	}
}

// BenchmarkYArray_RandomInsert_100k measures the cost of a single insert at a
// uniformly random index into a 100k-element YArray.
func BenchmarkYArray_RandomInsert_100k(b *testing.B) {
	b.ReportAllocs()

	doc, arr := buildHeavyArrayDoc(nHeavy)
	r := rand.New(rand.NewSource(42))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := r.Intn(arr.Len() + 1)
		doc.Transact(func(txn *Transaction) {
			arr.Insert(txn, idx, []any{i})
		})
	}
}

// BenchmarkYText_RandomInsert_100k measures the cost of a single
// single-character insert at a uniformly random position into a
// ~100k-character YText, exercising the search-marker positional-write path.
func BenchmarkYText_RandomInsert_100k(b *testing.B) {
	b.ReportAllocs()

	doc, txt := buildHeavyTextDoc(nHeavy)
	r := rand.New(rand.NewSource(42))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := r.Intn(txt.Len() + 1)
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, idx, "x", nil)
		})
	}
}

// BenchmarkYText_RandomDelete_100k measures the cost of deleting a single
// character at a uniformly random position into a ~100k-character YText.
func BenchmarkYText_RandomDelete_100k(b *testing.B) {
	b.ReportAllocs()

	doc, txt := buildHeavyTextDoc(nHeavy)
	r := rand.New(rand.NewSource(42))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if txt.Len() == 0 {
			// Guard against exhausting the doc across an unusually large b.N;
			// refill so the loop keeps measuring steady-state deletes.
			b.StopTimer()
			doc, txt = buildHeavyTextDoc(nHeavy)
			b.StartTimer()
		}
		idx := r.Intn(txt.Len())
		doc.Transact(func(txn *Transaction) {
			txt.Delete(txn, idx, 1)
		})
	}
}

// benchConflict simulates `users` independent peers, each performing
// opsPerUser concurrent inserts at the same start position (index 0) of a
// shared YText, then converging all peers via a single MergeUpdatesV1 +
// ApplyUpdateV1 round (mirrors BenchmarkTwoPeerConvergence but with N peers
// and heavier conflict rates — this is the B2/B3 scenario from
// dmonad/crdt-benchmarks).
func benchConflict(b *testing.B, users, opsPerUser int) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		docs := make([]*Doc, users)
		for u := range docs {
			docs[u] = New(WithClientID(ClientID(u + 1)))
			t := docs[u].GetText("t")
			docs[u].Transact(func(txn *Transaction) {
				for j := 0; j < opsPerUser; j++ {
					t.Insert(txn, 0, "x", nil)
				}
			})
		}

		updates := make([][]byte, users)
		for u, d := range docs {
			updates[u] = EncodeStateAsUpdateV1(d, nil)
		}

		merged, err := MergeUpdatesV1(updates...)
		if err != nil {
			b.Fatal(err)
		}

		for _, d := range docs {
			if err := ApplyUpdateV1(d, merged, nil); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// NOTE: same-start-position concurrent inserts are the pathological O(n^2)-ish
// conflict-arbitration case for a CRDT text type (every peer's insert is
// causally concurrent with, and ordered against, every other peer's insert at
// that position), which is exactly the shape B2/B3 are meant to exercise. The
// sizes below are deliberately bounded (not the dmonad/crdt-benchmarks
// upstream sizes) so a single -benchtime=1x iteration stays under ~3s and
// ~1GB allocated — nightly CI runs the benchheavy tier at -count=6 on
// GitHub-hosted runners with only ~7-16GB of RAM, and an unbounded conflict
// bench at that scale (e.g. 20 users x 1000 ops measured ~160s/iter and
// ~223GB allocated/iter) would OOM or hang the job. Profiling larger N is a
// manual `go test -bench` exercise outside CI, not something this suite runs
// automatically.

// BenchmarkConflict_B2_TwoUsers is the B2 scenario: two peers concurrently
// insert at the same position, then converge. Measured (this machine,
// -benchtime=1x -benchmem): ~12ms/iter, ~19.6MB/iter, ~21.9k allocs/iter.
func BenchmarkConflict_B2_TwoUsers(b *testing.B) { benchConflict(b, 2, 500) }

// BenchmarkConflict_B3_ManyUsers is the B3 scenario: many peers concurrently
// insert at the same position, then converge. Measured (this machine,
// -benchtime=1x -benchmem): ~413ms/iter, ~596MB/iter, ~274k allocs/iter.
func BenchmarkConflict_B3_ManyUsers(b *testing.B) { benchConflict(b, 10, 150) }
