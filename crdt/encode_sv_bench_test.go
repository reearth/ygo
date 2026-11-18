package crdt

import (
	"fmt"
	"runtime"
	"testing"
)

// BenchmarkEncodeStateAsUpdate_OneEditBehind encodes the update for a peer
// that lacks only the last keystroke, on a text whose history holds the given
// number of separate deletions (2 items per deletion).
func BenchmarkEncodeStateAsUpdate_OneEditBehind(b *testing.B) {
	for _, deletions := range []int{0, 1_000, 10_000} {
		doc, txt := fragmentedText(deletions)
		sv := doc.StateVector()
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "x", nil) })
		for _, enc := range []struct {
			name   string
			encode func(*Doc, StateVector) []byte
		}{{"V1", EncodeStateAsUpdateV1}, {"V2", EncodeStateAsUpdateV2}} {
			b.Run(fmt.Sprintf("deletions=%d/%s", deletions, enc.name), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					_ = enc.encode(doc, sv)
				}
			})
		}
	}
}

// BenchmarkOnUpdate_KeystrokeOnFragmentedText types one character at the
// start, with an OnUpdate observer registered, into a fresh copy of a text
// whose history holds the given number of separate deletions. The copy keeps
// the document from growing while it is measured, typing at the start keeps
// the copy's cold search marker out of the measurement, and the collection
// before each keystroke keeps the copy's garbage out of it.
func BenchmarkOnUpdate_KeystrokeOnFragmentedText(b *testing.B) {
	for _, deletions := range []int{0, 1_000, 10_000} {
		src, _ := fragmentedText(deletions)
		state := EncodeStateAsUpdateV1(src, nil)
		b.Run(fmt.Sprintf("deletions=%d", deletions), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				doc := newTestDoc(1)
				if err := ApplyUpdateV1(doc, state, nil); err != nil {
					b.Fatal(err)
				}
				txt := doc.GetText("t")
				doc.OnUpdate(func([]byte, any) {})
				runtime.GC()
				b.StartTimer()
				doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "x", nil) })
			}
		})
	}
}
