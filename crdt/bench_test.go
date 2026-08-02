package crdt

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// buildTextDoc creates a doc whose "text" YText contains n single-character
// inserts (one transaction per character, simulating keystroke-by-keystroke
// typing). The returned YText handle is safe to use outside Transact.
//
// This n-items-of-one-byte shape is a deliberate WORST CASE for any cost that
// scales with item count or per-string overhead (encoding, decoding,
// validation): maximum item/call count, with no string length to amortise a
// call's fixed overhead over. It is not representative of real documents,
// which tend to hold far fewer, much longer strings. Read a percentage off a
// benchmark built on this helper with that in mind, and prefer
// buildTextDocBulk below — which builds the same total byte count as a
// single bulk insert — wherever a realistic-shape comparison is needed.
func buildTextDoc(n int) (*Doc, *YText) { //nolint:unparam
	doc := newTestDoc(1)
	txt := doc.GetText("text")
	for i := 0; i < n; i++ {
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, txt.Len(), "a", nil)
		})
	}
	return doc, txt
}

// buildTextDocBulk creates a doc whose "text" YText holds the same total
// byte count as buildTextDoc(n), but inserted as a single n-byte string in
// one transaction instead of n one-character inserts in n separate
// transactions. It is the realistic-shape counterpart used by
// BenchmarkEncodeStateAsUpdateV1_Bulk below.
func buildTextDocBulk(n int) (*Doc, *YText) {
	doc := newTestDoc(1)
	txt := doc.GetText("text")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, strings.Repeat("a", n), nil)
	})
	return doc, txt
}

// BenchmarkYText_Insert measures the cost of appending a single character to a
// YText that already contains b.N-1 characters — i.e. each iteration extends a
// growing document by one keystroke.
func BenchmarkYText_Insert(b *testing.B) {
	b.ReportAllocs()

	doc := newTestDoc(1)
	txt := doc.GetText("text")

	// Pre-fill so that every measured iteration inserts into a non-empty doc.
	// We don't pre-fill here because the insert cost itself is what we measure,
	// and b.N drives the total size — that's the intended micro-benchmark.
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, txt.Len(), "a", nil)
		})
	}
}

// BenchmarkYText_InsertBulk measures inserting one large string in a single
// transaction rather than one character at a time.
func BenchmarkYText_InsertBulk(b *testing.B) {
	b.ReportAllocs()

	bulk := strings.Repeat("a", 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := newTestDoc(1)
		txt := doc.GetText("text")
		b.StartTimer()

		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, 0, bulk, nil)
		})
	}
}

// BenchmarkYText_Delete builds a 1000-character document once, then for each
// iteration deletes one character from position 0 until the document is empty,
// resetting between iterations.
func BenchmarkYText_Delete(b *testing.B) {
	b.ReportAllocs()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc, txt := buildTextDoc(1000)
		b.StartTimer()

		for txt.Len() > 0 {
			doc.Transact(func(txn *Transaction) {
				txt.Delete(txn, 0, 1)
			})
		}
	}
}

// BenchmarkEncodeStateAsUpdateV1 encodes a document that holds ~1000 YText
// characters into a V1 binary update.
func BenchmarkEncodeStateAsUpdateV1(b *testing.B) {
	b.ReportAllocs()

	doc, _ := buildTextDoc(1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EncodeStateAsUpdateV1(doc, nil)
	}
}

// BenchmarkEncodeStateAsUpdateV1_Bulk is the realistic-shape counterpart to
// BenchmarkEncodeStateAsUpdateV1 above: it encodes a document holding the
// same total byte count (1000 bytes of YText content), but built as a
// single bulk insert in one transaction rather than 1000 one-character
// inserts in 1000 separate transactions. BenchmarkEncodeStateAsUpdateV1's
// thousand-tiny-items shape is a deliberate worst case for per-string
// validation work — maximum validation call count, with no string length at
// all to amortise each call's fixed overhead over — not representative of
// real documents, which tend to hold far fewer, much longer strings. This
// benchmark measures that more typical shape instead.
func BenchmarkEncodeStateAsUpdateV1_Bulk(b *testing.B) {
	b.ReportAllocs()

	doc, _ := buildTextDocBulk(1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EncodeStateAsUpdateV1(doc, nil)
	}
}

// BenchmarkApplyUpdateV1 applies a pre-encoded V1 update (containing ~1000
// characters) to a fresh document on every iteration.
func BenchmarkApplyUpdateV1(b *testing.B) {
	b.ReportAllocs()

	src, _ := buildTextDoc(1000)
	update := EncodeStateAsUpdateV1(src, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := newTestDoc(2)
		if err := ApplyUpdateV1(dst, update, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApplyUpdateV1_Bulk is the realistic-shape counterpart to
// BenchmarkApplyUpdateV1 above: it applies an update built from the same
// total byte count (1000 bytes of YText content), but built as a single bulk
// insert in one transaction rather than 1000 one-character inserts in 1000
// separate transactions. BenchmarkApplyUpdateV1's thousand-tiny-items shape
// is a deliberate worst case for per-string validation work — maximum
// validation call count, with no string length at all to amortise each
// call's fixed overhead over — not representative of real documents, which
// tend to hold far fewer, much longer strings. This benchmark measures that
// more typical shape instead.
func BenchmarkApplyUpdateV1_Bulk(b *testing.B) {
	b.ReportAllocs()

	src, _ := buildTextDocBulk(1000)
	update := EncodeStateAsUpdateV1(src, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := newTestDoc(2)
		if err := ApplyUpdateV1(dst, update, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeStateAsUpdateV2 encodes a ~1000-character document in the V2
// column-oriented format.
func BenchmarkEncodeStateAsUpdateV2(b *testing.B) {
	b.ReportAllocs()

	doc, _ := buildTextDoc(1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EncodeStateAsUpdateV2(doc, nil)
	}
}

// BenchmarkEncodeStateAsUpdateV2_Bulk is the realistic-shape counterpart to
// BenchmarkEncodeStateAsUpdateV2 above: it encodes a document holding the
// same total byte count (1000 bytes of YText content), but built as a
// single bulk insert in one transaction rather than 1000 one-character
// inserts in 1000 separate transactions. BenchmarkEncodeStateAsUpdateV2's
// thousand-tiny-items shape is a deliberate worst case for per-string
// validation work — maximum validation call count, with no string length at
// all to amortise each call's fixed overhead over — not representative of
// real documents, which tend to hold far fewer, much longer strings. This
// benchmark measures that more typical shape instead.
func BenchmarkEncodeStateAsUpdateV2_Bulk(b *testing.B) {
	b.ReportAllocs()

	doc, _ := buildTextDocBulk(1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EncodeStateAsUpdateV2(doc, nil)
	}
}

// BenchmarkApplyUpdateV2 applies a pre-encoded V2 update (~1000 characters)
// to a fresh document on every iteration.
func BenchmarkApplyUpdateV2(b *testing.B) {
	b.ReportAllocs()

	src, _ := buildTextDoc(1000)
	update := EncodeStateAsUpdateV2(src, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := newTestDoc(2)
		if err := ApplyUpdateV2(dst, update, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApplyUpdateV2_Bulk is the realistic-shape counterpart to
// BenchmarkApplyUpdateV2 above: it applies an update built from the same
// total byte count (1000 bytes of YText content), but built as a single bulk
// insert in one transaction rather than 1000 one-character inserts in 1000
// separate transactions. BenchmarkApplyUpdateV2's thousand-tiny-items shape
// is a deliberate worst case for per-string validation work — maximum
// validation call count, with no string length at all to amortise each
// call's fixed overhead over — not representative of real documents, which
// tend to hold far fewer, much longer strings. This benchmark measures that
// more typical shape instead.
func BenchmarkApplyUpdateV2_Bulk(b *testing.B) {
	b.ReportAllocs()

	src, _ := buildTextDocBulk(1000)
	update := EncodeStateAsUpdateV2(src, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := newTestDoc(2)
		if err := ApplyUpdateV2(dst, update, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMergeUpdatesV1 encodes 10 small V1 updates (100 chars each) and
// then merges them into a single update on each iteration.
func BenchmarkMergeUpdatesV1(b *testing.B) {
	b.ReportAllocs()

	const numUpdates = 10
	const charsPerUpdate = 100

	// Build 10 independent documents, each with 100 chars, and capture their
	// updates. Using distinct client IDs ensures no clock collisions.
	updates := make([][]byte, numUpdates)
	for i := 0; i < numUpdates; i++ {
		doc, _ := buildTextDocWithClient(uint64(i+1), charsPerUpdate)
		updates[i] = EncodeStateAsUpdateV1(doc, nil)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		merged, err := MergeUpdatesV1(updates...)
		if err != nil {
			b.Fatal(err)
		}
		_ = merged
	}
}

// buildTextDocWithClient is like buildTextDoc but lets callers specify the
// client ID, which avoids clock collisions when building multiple peer docs.
func buildTextDocWithClient(clientID uint64, n int) (*Doc, *YText) {
	doc := New(WithClientID(ClientID(clientID)))
	txt := doc.GetText("text")
	for i := 0; i < n; i++ {
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, txt.Len(), "a", nil)
		})
	}
	return doc, txt
}

// BenchmarkYMap_Set sets 100 distinct keys inside a single transaction on each
// iteration, starting from a fresh document.
func BenchmarkYMap_Set(b *testing.B) {
	b.ReportAllocs()

	const numKeys = 100
	keys := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		keys[i] = fmt.Sprintf("key-%d", i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := newTestDoc(1)
		m := doc.GetMap("map")
		b.StartTimer()

		doc.Transact(func(txn *Transaction) {
			for _, k := range keys {
				m.Set(txn, k, "value")
			}
		})
	}
}

// BenchmarkYArray_Push pushes 100 elements (one per transaction) into an array
// on each iteration.
func BenchmarkYArray_Push(b *testing.B) {
	b.ReportAllocs()

	const numElems = 100

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := newTestDoc(1)
		arr := doc.GetArray("arr")
		b.StartTimer()

		for j := 0; j < numElems; j++ {
			doc.Transact(func(txn *Transaction) {
				arr.Push(txn, []any{j})
			})
		}
	}
}

// BenchmarkTwoPeerConvergence simulates a full sync round-trip: Alice types
// 100 characters, encodes her state, and Bob applies it. This is the canonical
// "two-peer convergence" pattern.
func BenchmarkTwoPeerConvergence(b *testing.B) {
	b.ReportAllocs()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()

		// Alice builds her document outside the measured section.
		alice := newTestDoc(1)
		aliceTxt := alice.GetText("text")
		for j := 0; j < 100; j++ {
			alice.Transact(func(txn *Transaction) {
				aliceTxt.Insert(txn, aliceTxt.Len(), "a", nil)
			})
		}
		update := EncodeStateAsUpdateV1(alice, nil)

		bob := newTestDoc(2)

		b.StartTimer()

		// Measured: encode (already done above, cost attributed to Alice's side)
		// and apply to Bob.
		if err := ApplyUpdateV1(bob, update, nil); err != nil {
			b.Fatal(err)
		}

		// Bob makes a local edit and syncs back to Alice.
		bobTxt := bob.GetText("text")
		bob.Transact(func(txn *Transaction) {
			bobTxt.Insert(txn, bobTxt.Len(), "b", nil)
		})
		bobUpdate := EncodeStateAsUpdateV1(bob, alice.StateVector())
		if err := ApplyUpdateV1(alice, bobUpdate, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkConcurrentSamePositionInsert stresses the YATA conflict-scan slow
// path in Item.integrate (issue #54-C). N peers each insert one element at
// position 0 of an empty doc, so every item shares Origin=nil and OriginRight=nil
// — the maximal-contention case. Converging all N updates into one doc forces
// integrate to walk a conflict group that grows toward N, which is exactly where
// the `conflicting` / `beforeOrigin` maps (item.go) are allocated and reset.
// Measure this before deciding whether pooling those maps is worthwhile.
func BenchmarkConcurrentSamePositionInsert(b *testing.B) {
	for _, peers := range []int{20, 100, 400} {
		b.Run(fmt.Sprintf("peers=%d", peers), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				updates := make([][]byte, peers)
				for p := 0; p < peers; p++ {
					d := New(WithClientID(ClientID(p + 1)))
					t := d.GetText("t")
					d.Transact(func(txn *Transaction) { t.Insert(txn, 0, "x", nil) })
					updates[p] = EncodeStateAsUpdateV1(d, nil)
				}
				merged := New(WithClientID(999999))
				b.StartTimer()

				for _, u := range updates {
					if err := ApplyUpdateV1(merged, u, nil); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Search-marker proving harness (Task 8, #181).
//
// Tasks 1-7 replaced the old forward-only posCache with a move-aware,
// Yjs-style search marker (crdt/search_marker.go). abstractType.disableMarkers
// is the test seam those tasks added: forcing it true makes every positional
// lookup fall back to the pre-marker full linear walk from t.start, exactly
// as it behaved before. Every benchmark below runs the identical workload
// twice — once with markers live (the "_Markers" variant) and once with
// disableMarkers forced true (the "_Cold" variant) — so the ns/op columns
// are a direct, apples-to-apples measurement of the speedup those tasks
// claim, on the shapes that used to be O(n) per op (random access) or
// O(n^2) over a growing/shrinking document (many such ops in a row).
//
// Base documents are always CONSTRUCTED with markers enabled — disableMarkers
// is flipped only afterward, immediately before the timed section. This
// keeps setup cost identical (and cheap) between the two variants; building
// a large fragmented document with disableMarkers forced from the start
// would itself be the O(n^2) cold walk the benchmark exists to measure,
// which would make the 100k builds impractically slow.
// ---------------------------------------------------------------------------

// buildFragmentedTextDoc builds a YText of exactly target characters by
// inserting one character at a time at a random position, each in its own
// transaction. Two things matter about this shape:
//
//   - One transaction per character means squashRuns (transaction.go) — which
//     only merges items created within the SAME transaction — never collapses
//     them back together, so the linked list stays genuinely fragmented into
//     target separate Items. That is what makes a cold linear walk from
//     t.start expensive.
//   - Every item has length exactly 1, so a later Insert at any position
//     always lands ON an item boundary (leftNeighbourAt's offset is always
//     == len) and never INSIDE the middle of a multi-character item. That
//     matters: a mid-item insert forces splitItem, which itself conservatively
//     clears every marker (item.go's splitItem doc comment) and does an O(store
//     size) StructStore.insertItem slice-shift — a cost that has nothing to do
//     with search markers but would otherwise dominate and mask the very
//     speedup these benchmarks exist to measure. Single-character items avoid
//     that confound entirely, isolating the position-lookup cost.
//
// Markers are left enabled during this build (the default zero value of
// disableMarkers), keeping construction fast regardless of which variant
// calls it.
func buildFragmentedTextDoc(target int, seed int64) (*Doc, *YText) {
	doc := newTestDoc(1)
	txt := doc.GetText("text")
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < target; i++ {
		pos := rng.Intn(txt.Len() + 1)
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, pos, "a", nil)
		})
	}
	return doc, txt
}

// buildFragmentedArrayDoc builds a YArray of exactly target elements
// (values 0..target-1, each its own Insert call at a random position), the
// YArray counterpart of buildFragmentedTextDoc above.
func buildFragmentedArrayDoc(target int, seed int64) (*Doc, *YArray) {
	doc := newTestDoc(1)
	arr := doc.GetArray("arr")
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < target; i++ {
		pos := rng.Intn(arr.Len() + 1)
		doc.Transact(func(txn *Transaction) {
			arr.Insert(txn, pos, []any{i})
		})
	}
	return doc, arr
}

// benchInsertRandom measures inserting a single character at a
// uniformly-random position on every iteration — the classic O(n)-per-op
// cold case, since a linear walk from t.start must cross, on average, half
// the document to resolve a random target index.
func benchInsertRandom(b *testing.B, n int, cold bool) {
	b.ReportAllocs()

	doc, txt := buildFragmentedTextDoc(n, int64(n)+1)
	txt.baseType().disableMarkers = cold
	rng := rand.New(rand.NewSource(int64(n) + 2))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pos := rng.Intn(txt.Len() + 1)
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, pos, "x", nil)
		})
	}
}

// benchInsertReverse measures inserting characters at a cursor that walks
// BACKWARD one position per iteration through a large pre-existing document
// (decrementing from txt.Len() toward 0, wrapping back to the end when it
// hits bottom). This is the adversarial case for a forward-only cache: each
// new target is strictly less than the previous one, so a cache that only
// ever remembers "the last position, moving forward" is useless here and
// every op degrades to a full cold walk. The move-aware marker this task
// benchmarks resolves it in O(1) per step instead, via walkLeftFrom from the
// marker installed one position to the right on the previous iteration —
// this is exactly the "move-aware" (bidirectional) property Tasks 1-7 added
// over the old posCache.
func benchInsertReverse(b *testing.B, n int, cold bool) {
	b.ReportAllocs()

	doc, txt := buildFragmentedTextDoc(n, int64(n)+3)
	txt.baseType().disableMarkers = cold
	cursor := txt.Len()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if cursor < 0 {
			cursor = txt.Len()
		}
		pos := cursor
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, pos, "x", nil)
		})
		cursor--
	}
}

// benchArrayGetRandom measures YArray.Get at a uniformly-random index on
// every iteration — the YArray counterpart of benchInsertRandom. Get must be
// called outside Transact (it takes its own read lock).
func benchArrayGetRandom(b *testing.B, n int, cold bool) {
	b.ReportAllocs()

	doc, arr := buildFragmentedArrayDoc(n, int64(n)+4)
	_ = doc
	arr.baseType().disableMarkers = cold
	rng := rand.New(rand.NewSource(int64(n) + 5))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := rng.Intn(arr.Len())
		_ = arr.Get(idx)
	}
}

// benchDeleteRangeTail measures deleting a small chunk from just before the
// end of a large document on every iteration. The tail is the position
// farthest from t.start, so a cold walk must cross the entire document to
// reach it every single time.
func benchDeleteRangeTail(b *testing.B, n int, cold bool) {
	b.ReportAllocs()

	const chunk = 3
	doc, txt := buildFragmentedTextDoc(n, int64(n)+6)
	txt.baseType().disableMarkers = cold

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := txt.Len()
		if l <= chunk {
			break
		}
		pos := l - chunk
		doc.Transact(func(txn *Transaction) {
			txt.Delete(txn, pos, chunk)
		})
	}
}

// benchDeleteRangeRandom measures deleting a small chunk at a
// uniformly-random position on every iteration.
func benchDeleteRangeRandom(b *testing.B, n int, cold bool) {
	b.ReportAllocs()

	const chunk = 3
	doc, txt := buildFragmentedTextDoc(n, int64(n)+7)
	txt.baseType().disableMarkers = cold
	rng := rand.New(rand.NewSource(int64(n) + 8))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := txt.Len()
		if l <= chunk {
			break
		}
		pos := rng.Intn(l - chunk)
		doc.Transact(func(txn *Transaction) {
			txt.Delete(txn, pos, chunk)
		})
	}
}

// benchApplyDelta measures applying a multi-op (retain/delete/insert) delta
// — the same shape a rich-text editor emits for a single user edit — to a
// large document on every iteration. ApplyDelta advances a single running
// cursor through its ops (search_marker_test.go's Task 4 header comment),
// so this exercises the marker-accelerated cursor advance across a Retain
// large enough to force a real positional jump on a big document.
func benchApplyDelta(b *testing.B, n int, cold bool) {
	b.ReportAllocs()

	const insertStr = "the quick brown fox jumps"
	const delChunk = 5
	doc, txt := buildFragmentedTextDoc(n, int64(n)+9)
	txt.baseType().disableMarkers = cold

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := txt.Len()
		retain := l / 2
		del := delChunk
		if retain+del > l {
			del = 0
		}
		delta := []Delta{
			{Op: DeltaOpRetain, Retain: retain},
			{Op: DeltaOpDelete, Delete: del},
			{Op: DeltaOpInsert, Insert: insertStr},
		}
		doc.Transact(func(txn *Transaction) {
			txt.ApplyDelta(txn, delta)
		})
	}
}

func BenchmarkSearchMarker_InsertRandom_1k_Markers(b *testing.B) { benchInsertRandom(b, 1_000, false) }
func BenchmarkSearchMarker_InsertRandom_1k_Cold(b *testing.B)    { benchInsertRandom(b, 1_000, true) }
func BenchmarkSearchMarker_InsertRandom_100k_Markers(b *testing.B) {
	benchInsertRandom(b, 100_000, false)
}
func BenchmarkSearchMarker_InsertRandom_100k_Cold(b *testing.B) { benchInsertRandom(b, 100_000, true) }

func BenchmarkSearchMarker_InsertReverse_1k_Markers(b *testing.B) {
	benchInsertReverse(b, 1_000, false)
}
func BenchmarkSearchMarker_InsertReverse_1k_Cold(b *testing.B) { benchInsertReverse(b, 1_000, true) }
func BenchmarkSearchMarker_InsertReverse_100k_Markers(b *testing.B) {
	benchInsertReverse(b, 100_000, false)
}
func BenchmarkSearchMarker_InsertReverse_100k_Cold(b *testing.B) {
	benchInsertReverse(b, 100_000, true)
}

func BenchmarkSearchMarker_ArrayGetRandom_1k_Markers(b *testing.B) {
	benchArrayGetRandom(b, 1_000, false)
}
func BenchmarkSearchMarker_ArrayGetRandom_1k_Cold(b *testing.B) {
	benchArrayGetRandom(b, 1_000, true)
}
func BenchmarkSearchMarker_ArrayGetRandom_100k_Markers(b *testing.B) {
	benchArrayGetRandom(b, 100_000, false)
}
func BenchmarkSearchMarker_ArrayGetRandom_100k_Cold(b *testing.B) {
	benchArrayGetRandom(b, 100_000, true)
}

func BenchmarkSearchMarker_DeleteRangeTail_1k_Markers(b *testing.B) {
	benchDeleteRangeTail(b, 1_000, false)
}
func BenchmarkSearchMarker_DeleteRangeTail_1k_Cold(b *testing.B) {
	benchDeleteRangeTail(b, 1_000, true)
}
func BenchmarkSearchMarker_DeleteRangeTail_100k_Markers(b *testing.B) {
	benchDeleteRangeTail(b, 100_000, false)
}
func BenchmarkSearchMarker_DeleteRangeTail_100k_Cold(b *testing.B) {
	benchDeleteRangeTail(b, 100_000, true)
}

func BenchmarkSearchMarker_DeleteRangeRandom_1k_Markers(b *testing.B) {
	benchDeleteRangeRandom(b, 1_000, false)
}
func BenchmarkSearchMarker_DeleteRangeRandom_1k_Cold(b *testing.B) {
	benchDeleteRangeRandom(b, 1_000, true)
}
func BenchmarkSearchMarker_DeleteRangeRandom_100k_Markers(b *testing.B) {
	benchDeleteRangeRandom(b, 100_000, false)
}
func BenchmarkSearchMarker_DeleteRangeRandom_100k_Cold(b *testing.B) {
	benchDeleteRangeRandom(b, 100_000, true)
}

func BenchmarkSearchMarker_ApplyDelta_1k_Markers(b *testing.B) { benchApplyDelta(b, 1_000, false) }
func BenchmarkSearchMarker_ApplyDelta_1k_Cold(b *testing.B)    { benchApplyDelta(b, 1_000, true) }
func BenchmarkSearchMarker_ApplyDelta_100k_Markers(b *testing.B) {
	benchApplyDelta(b, 100_000, false)
}
func BenchmarkSearchMarker_ApplyDelta_100k_Cold(b *testing.B) { benchApplyDelta(b, 100_000, true) }

// ---------------------------------------------------------------------------
// Light correctness re-assertion for the helpers above.
//
// search_marker_test.go already carries heavy, dedicated fuzz-style coverage
// proving markers agree with the cold oracle across inserts, deletes, Get,
// Slice, and ApplyDelta. The two tests below are deliberately light — they
// exist only to guard the NEW builder/workload helpers in this file (which
// are not exercised anywhere else) against a silent divergence, so a benchmark
// showing "markers are faster" is never quietly measuring two paths that
// disagree.
// ---------------------------------------------------------------------------

// TestBenchSearchMarker_TextHelpersMatchCold runs the exact insert-random,
// insert-reverse, delete-range, and ApplyDelta sequences the benchmarks above
// use — once with markers enabled, once with disableMarkers forced — and
// asserts the resulting document text is byte-identical either way.
func TestBenchSearchMarker_TextHelpersMatchCold(t *testing.T) {
	const n = 2000
	run := func(cold bool) string {
		doc, txt := buildFragmentedTextDoc(n, 99)
		txt.baseType().disableMarkers = cold

		rng := rand.New(rand.NewSource(100))
		for i := 0; i < 200; i++ {
			pos := rng.Intn(txt.Len() + 1)
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, pos, "x", nil) })
		}

		cursor := txt.Len()
		for i := 0; i < 200; i++ {
			if cursor < 0 {
				cursor = txt.Len()
			}
			pos := cursor
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, pos, "y", nil) })
			cursor--
		}

		for i := 0; i < 50; i++ {
			l := txt.Len()
			if l <= 5 {
				break
			}
			pos := rng.Intn(l - 5)
			doc.Transact(func(txn *Transaction) { txt.Delete(txn, pos, 5) })
		}

		l := txt.Len()
		retain := l / 2
		doc.Transact(func(txn *Transaction) {
			txt.ApplyDelta(txn, []Delta{
				{Op: DeltaOpRetain, Retain: retain},
				{Op: DeltaOpDelete, Delete: 5},
				{Op: DeltaOpInsert, Insert: "zzz"},
			})
		})
		return txt.ToString()
	}

	got, want := run(false), run(true)
	if got != want {
		t.Fatalf("marker/cold divergence: len(got)=%d len(want)=%d", len(got), len(want))
	}
}

// TestBenchSearchMarker_ArrayGetHelperMatchesCold is the YArray.Get
// counterpart of the text test above.
func TestBenchSearchMarker_ArrayGetHelperMatchesCold(t *testing.T) {
	const n = 2000
	idxs := []int{0, 1, 999, 1000, 1999}
	run := func(cold bool) []any {
		doc, arr := buildFragmentedArrayDoc(n, 101)
		_ = doc
		arr.baseType().disableMarkers = cold
		out := make([]any, 0, len(idxs))
		for _, idx := range idxs {
			out = append(out, arr.Get(idx))
		}
		return out
	}

	got, want := run(false), run(true)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("marker/cold divergence:\n got  %v\n want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Engine light-tier slow-path benchmarks (Task 1, #180).
//
// These mirror the dmonad/crdt-benchmarks B1 scenario shapes (append is
// already covered by BenchmarkYText_Insert above; prepend/random/word/
// insert-then-delete/mixed follow here) at light-tier sizes (nLight ops,
// fixed-seed RNG) so the PR gate stays fast and deterministic. Heavy-tier
// (100k) equivalents live behind the benchheavy build tag elsewhere.
// ---------------------------------------------------------------------------

const benchSeed = 42
const nLight = 2000

// BenchmarkYText_Prepend inserts at index 0 repeatedly — the worst case for a
// forward-only cursor/cache, since every insert lands behind everything
// already in the document.
func BenchmarkYText_Prepend(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		doc.Transact(func(txn *Transaction) {
			for j := 0; j < nLight; j++ {
				txt.Insert(txn, 0, "x", nil)
			}
		})
	}
}

// BenchmarkYText_RandomInsert inserts nLight characters at seeded-random
// positions into a growing document.
func BenchmarkYText_RandomInsert(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		r := rand.New(rand.NewSource(benchSeed))
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		doc.Transact(func(txn *Transaction) {
			for j := 0; j < nLight; j++ {
				txt.Insert(txn, r.Intn(txt.Len()+1), "x", nil)
			}
		})
	}
}

// BenchmarkYText_InsertThenDelete inserts nLight characters at seeded-random
// positions, then deletes them one at a time from seeded-random positions
// until the document is empty again.
func BenchmarkYText_InsertThenDelete(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		r := rand.New(rand.NewSource(benchSeed))
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		doc.Transact(func(txn *Transaction) {
			for j := 0; j < nLight; j++ {
				txt.Insert(txn, r.Intn(txt.Len()+1), "x", nil)
			}
		})
		for txt.Len() > 0 {
			doc.Transact(func(txn *Transaction) {
				txt.Delete(txn, r.Intn(txt.Len()), 1)
			})
		}
	}
}

// BenchmarkYText_WordInsert inserts nLight short "words" at the current end
// of the document, one Transact per word.
func BenchmarkYText_WordInsert(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		for j := 0; j < nLight; j++ {
			doc.Transact(func(txn *Transaction) {
				txt.Insert(txn, txt.Len(), "lorem ", nil)
			})
		}
	}
}

// BenchmarkYText_MixedEdits runs a seeded walk of nLight steps: 70% insert a
// character at a random position, 30% delete one character at a random
// position (an insert is forced whenever the document is empty).
func BenchmarkYText_MixedEdits(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		r := rand.New(rand.NewSource(benchSeed))
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		doc.Transact(func(txn *Transaction) {
			for j := 0; j < nLight; j++ {
				if txt.Len() == 0 || r.Float64() < 0.7 {
					txt.Insert(txn, r.Intn(txt.Len()+1), "x", nil)
				} else {
					txt.Delete(txn, r.Intn(txt.Len()), 1)
				}
			}
		})
	}
}

// BenchmarkYText_Format seeds an nLight-character document once (outside the
// timed loop, so setup cost isn't attributed to the format op) and then
// repeatedly formats a random 10-character span within it.
func BenchmarkYText_Format(b *testing.B) {
	b.ReportAllocs()

	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, strings.Repeat("x", nLight), nil)
	})
	r := rand.New(rand.NewSource(benchSeed))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc.Transact(func(txn *Transaction) {
			txt.Format(txn, r.Intn(txt.Len()-10), 10, Attributes{"bold": true})
		})
	}
}

// BenchmarkYText_ApplyDelta applies a fixed retain/delete/insert/format delta
// (the shape a rich-text editor emits for a single user edit) to a fresh
// small document on every iteration.
func BenchmarkYText_ApplyDelta(b *testing.B) {
	b.ReportAllocs()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, 0, "Hello World", nil)
		})
		b.StartTimer()

		doc.Transact(func(txn *Transaction) {
			txt.ApplyDelta(txn, []Delta{
				{Op: DeltaOpRetain, Retain: 6},
				{Op: DeltaOpDelete, Delete: 5},
				{Op: DeltaOpInsert, Insert: "Go", Attributes: Attributes{"bold": true}},
			})
		})
	}
}

// benchObservedTxn measures the cost of a single-character append transaction
// with (withObserver=true) and without an active YText.Observe subscriber.
// The delta between BenchmarkObservedTxn_Apply and _ApplyBaseline IS the
// signal: it isolates the marginal cost of computing and dispatching the
// observer event on every transaction commit.
func benchObservedTxn(b *testing.B, withObserver bool) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "seed", nil) })
	if withObserver {
		unsub := txt.Observe(func(YTextEvent) {}) // minimal observer
		defer unsub()
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "x", nil) })
	}
}

func BenchmarkObservedTxn_Apply(b *testing.B)         { benchObservedTxn(b, true) }
func BenchmarkObservedTxn_ApplyBaseline(b *testing.B) { benchObservedTxn(b, false) }
