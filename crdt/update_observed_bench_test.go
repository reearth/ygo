package crdt

import "testing"

// BenchmarkApplyUpdateV1_Observed applies 500 keystroke updates, one per
// iteration, to a document with an OnUpdate observer: each one new (fresh),
// or each one the document already holds (resent).
func BenchmarkApplyUpdateV1_Observed(b *testing.B) {
	src := newTestDoc(1)
	txt := src.GetText("t")
	var updates [][]byte
	unsub := src.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	for i := range 500 {
		src.Transact(func(txn *Transaction) { txt.Insert(txn, i, "x", nil) })
	}
	unsub()
	full := EncodeStateAsUpdateV1(src, nil)

	for _, resent := range []bool{false, true} {
		name := "fresh"
		if resent {
			name = "resent"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			var dst *Doc
			for i := range b.N {
				if i%len(updates) == 0 {
					b.StopTimer()
					dst = newTestDoc(2)
					if resent {
						if err := ApplyUpdateV1(dst, full, nil); err != nil {
							b.Fatal(err)
						}
					}
					dst.OnUpdate(func([]byte, any) {})
					b.StartTimer()
				}
				if err := ApplyUpdateV1(dst, updates[i%len(updates)], nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
