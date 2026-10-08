package crdt

import (
	"fmt"
	"testing"
)

const toJSONBenchSize = 10_000

// toJSONBenchLeaves are the element shapes for the flat ToJSON benchmarks:
// floats and small objects, the common values that integer-leaf benchmarks
// miss, and integers for comparison.
var toJSONBenchLeaves = []struct {
	name  string
	value func(i int) any
}{
	{"float", func(i int) any { return float64(i) + 0.5 }},
	{"object", func(i int) any { return map[string]any{"id": float64(i), "name": "item"} }},
	{"int", func(i int) any { return i }},
}

// BenchmarkYArray_ToJSON serialises a flat 10,000-element array.
func BenchmarkYArray_ToJSON(b *testing.B) {
	for _, leaf := range toJSONBenchLeaves {
		b.Run(leaf.name, func(b *testing.B) {
			doc := newTestDoc(1)
			arr := doc.GetArray("a")
			vals := make([]any, toJSONBenchSize)
			for i := range vals {
				vals[i] = leaf.value(i)
			}
			doc.Transact(func(txn *Transaction) { arr.Push(txn, vals) })

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := arr.ToJSON(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkYMap_ToJSON serialises a flat 10,000-key map.
func BenchmarkYMap_ToJSON(b *testing.B) {
	for _, leaf := range toJSONBenchLeaves {
		b.Run(leaf.name, func(b *testing.B) {
			doc := newTestDoc(1)
			m := doc.GetMap("m")
			doc.Transact(func(txn *Transaction) {
				for i := range toJSONBenchSize {
					m.Set(txn, fmt.Sprintf("k%05d", i), leaf.value(i))
				}
			})

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := m.ToJSON(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
