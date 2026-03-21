package crdt

import (
	"fmt"
	"strings"
	"testing"
)

// buildTextDoc creates a doc whose "text" YText contains n single-character
// inserts (one transaction per character, simulating keystroke-by-keystroke
// typing). The returned YText handle is safe to use outside Transact.
func buildTextDoc(n int) (*Doc, *YText) {
	doc := newTestDoc(1)
	txt := doc.GetText("text")
	for i := 0; i < n; i++ {
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, txt.Len(), "a", nil)
		})
	}
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
