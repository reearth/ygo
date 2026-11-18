package crdt

import "testing"

// BenchmarkOnUpdate_Transact measures a transaction with an OnUpdate
// subscriber: a local keystroke, a local insert and delete of the same text,
// and a remote apply into a text with an observer.
func BenchmarkOnUpdate_Transact(b *testing.B) {
	b.Run("local-keystroke", func(b *testing.B) {
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		doc.OnUpdate(func([]byte, any) {})
		b.ReportAllocs()
		for range b.N {
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "x", nil) })
		}
	})
	b.Run("local-insert-delete", func(b *testing.B) {
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		doc.OnUpdate(func([]byte, any) {})
		b.ReportAllocs()
		for range b.N {
			doc.Transact(func(txn *Transaction) {
				n := txt.Len()
				txt.Insert(txn, n, "hello", nil)
				txt.Delete(txn, n+1, 3)
			})
		}
	})
	b.Run("remote-observed-text", func(b *testing.B) {
		src := newTestDoc(2)
		st := src.GetText("t")
		var updates [][]byte
		src.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
		for i := range 1000 {
			src.Transact(func(txn *Transaction) { st.Insert(txn, i, "x", nil) })
		}
		src.Transact(func(txn *Transaction) { st.Delete(txn, 100, 800) })
		b.ReportAllocs()
		for range b.N {
			b.StopTimer()
			doc := newTestDoc(1)
			doc.GetText("t").Observe(func(YTextEvent) {})
			doc.OnUpdate(func([]byte, any) {})
			b.StartTimer()
			for _, u := range updates {
				if err := ApplyUpdateV1(doc, u, nil); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}
