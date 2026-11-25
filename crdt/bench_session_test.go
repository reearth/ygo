package crdt

import (
	"math/rand"
	"runtime"
	"testing"
	"time"
)

// The micro-benchmarks above time one operation on a prepared document. A
// struct merge is not visible there: it costs a split on the next delete and
// repays it on every encode, apply and later insert. These replay a whole
// editing session instead — single-character edits, one transaction each, as
// an editor sends them — and report both what the session costs and what it
// leaves behind, so a change that trades one for the other is legible.
//
//	caretPct - ops that move the caret elsewhere, breaking the run
//	delPct   - ops that delete a short contiguous run at the caret
func benchSession(b *testing.B, ops, caretPct, delPct int) {
	b.Helper()
	var state, structs, load, heap float64
	b.ResetTimer()
	for range b.N {
		rng := rand.New(rand.NewSource(42))
		doc := New(WithClientID(1))
		txt := doc.GetText("text")
		caret := 0
		for range ops {
			switch r := rng.Intn(100); {
			case r < caretPct && txt.Len() > 0:
				caret = rng.Intn(txt.Len() + 1)
			case r < caretPct+delPct && txt.Len() > 16:
				n := 1 + rng.Intn(8)
				if caret < n {
					caret = n
				}
				from := caret - n
				doc.Transact(func(txn *Transaction) { txt.Delete(txn, from, n) })
				caret = from
			default:
				at := min(caret, txt.Len())
				doc.Transact(func(txn *Transaction) { txt.Insert(txn, at, "a", nil) })
				caret = at + 1
			}
		}

		b.StopTimer()
		update := EncodeStateAsUpdateV1(doc, nil)
		state, structs = float64(len(update)), float64(len(doc.store.clients[1]))

		start := time.Now()
		fresh := New(WithClientID(2))
		if err := ApplyUpdateV1(fresh, update, nil); err != nil {
			b.Fatal(err)
		}
		load = float64(time.Since(start).Nanoseconds())

		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		heap = float64(ms.HeapAlloc)
		runtime.KeepAlive(fresh)
		b.StartTimer()
	}
	b.ReportMetric(state, "state-bytes")
	b.ReportMetric(structs, "structs")
	b.ReportMetric(load, "load-ns")
	b.ReportMetric(heap, "heap-bytes")
}

// Typing runs straight: every item is contiguous with the one before it.
func BenchmarkSession_Typing(b *testing.B) { benchSession(b, 5000, 0, 0) }

// Prose: typing, backspacing, and the occasional jump to another paragraph.
func BenchmarkSession_Prose(b *testing.B) { benchSession(b, 5000, 2, 15) }

// A revision pass over a draft, where most of the work is deletion.
func BenchmarkSession_DeleteHeavy(b *testing.B) { benchSession(b, 5000, 2, 40) }
