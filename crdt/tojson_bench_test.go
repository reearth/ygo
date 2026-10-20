package crdt

import (
	"encoding/json"
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
	{"mixed", func(i int) any {
		if i%2 == 0 {
			return map[string]any{"id": float64(i)}
		}
		return float64(i) + 0.5
	}},
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

// BenchmarkYMap_Entries_Texts marshals the entries of a map of 1,000 YTexts,
// a reader that converts nested leaf types in place.
func BenchmarkYMap_Entries_Texts(b *testing.B) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		for i := range 1000 {
			text := NewTextPrelim()
			m.Set(txn, fmt.Sprintf("k%04d", i), text)
			text.Insert(txn, 0, "hello", nil)
		}
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := json.Marshal(m.Entries()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkYArray_ToSlice_Maps reads an array of 1,000 nested maps.
func BenchmarkYArray_ToSlice_Maps(b *testing.B) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		for i := range 1000 {
			nested := NewMapPrelim()
			arr.PushType(txn, nested)
			nested.Set(txn, "id", float64(i))
		}
	})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if len(arr.ToSlice()) != 1000 {
			b.Fatal("short slice")
		}
	}
}
