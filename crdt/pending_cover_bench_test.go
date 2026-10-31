package crdt

import (
	"fmt"
	"math"
	"testing"
)

// parkedTailFixture is a stored state holding n structs of client 2 parked
// behind client 9's missing "ab", typed one keystroke at a time, the next
// keystroke after them, and client 9's "ab".
func parkedTailFixture(b *testing.B, n int) (state, next, ab []byte) {
	b.Helper()
	other := New(WithClientID(9))
	other.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "ab", nil) })
	ab = EncodeStateAsUpdateV1(other, nil)
	writer := New(WithClientID(2))
	if err := ApplyUpdateV1(writer, ab, nil); err != nil {
		b.Fatal(err)
	}
	room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
	for i := range n + 1 {
		sv := writer.StateVector()
		writer.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 2+i, "x", nil) })
		u := EncodeStateAsUpdateV1(writer, sv)
		if i == n {
			next = u
			break
		}
		if err := ApplyUpdateV1(room, u, nil); err != nil {
			b.Fatal(err)
		}
	}
	return EncodeStateAsUpdateV1(room, nil), next, ab
}

func loadParked(b *testing.B, state []byte, n int) *Doc {
	b.Helper()
	doc := New(WithClientID(3), WithMaxPendingItems(math.MaxInt))
	if err := ApplyUpdateV1(doc, state, nil); err != nil {
		b.Fatal(err)
	}
	if got := doc.PendingStats().Items; got != n {
		b.Fatalf("%d items parked, want %d", got, n)
	}
	return doc
}

// BenchmarkPendingPark times the park path: one keystroke that parks, applied
// to a document already holding n parked structs.
func BenchmarkPendingPark(b *testing.B) {
	for _, n := range []int{1_000, 20_000} {
		b.Run(fmt.Sprintf("parked=%d", n), func(b *testing.B) {
			state, next, _ := parkedTailFixture(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				doc := loadParked(b, state, n)
				b.StartTimer()
				if err := ApplyUpdateV1(doc, next, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPendingDrain times the drain path: the missing predecessor arrives
// at a document holding n parked structs, which all integrate.
func BenchmarkPendingDrain(b *testing.B) {
	for _, n := range []int{1_000, 20_000} {
		b.Run(fmt.Sprintf("parked=%d", n), func(b *testing.B) {
			state, _, ab := parkedTailFixture(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				doc := loadParked(b, state, n)
				b.StartTimer()
				if err := ApplyUpdateV1(doc, ab, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPendingKeystrokeBesideParked times a keystroke of another client,
// which parks nothing, on a document holding n parked structs.
func BenchmarkPendingKeystrokeBesideParked(b *testing.B) {
	for _, n := range []int{1_000, 20_000} {
		b.Run(fmt.Sprintf("parked=%d", n), func(b *testing.B) {
			state, _, _ := parkedTailFixture(b, n)
			typist := New(WithClientID(5))
			doc := loadParked(b, state, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				sv := typist.StateVector()
				typist.Transact(func(txn *Transaction) { txn.GetText("u").Insert(txn, 0, "k", nil) })
				u := EncodeStateAsUpdateV1(typist, sv)
				b.StartTimer()
				if err := ApplyUpdateV1(doc, u, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPendingParkSteady times the park path once the queue's index is
// built: each iteration parks one more keystroke after n parked structs.
func BenchmarkPendingParkSteady(b *testing.B) {
	for _, n := range []int{1_000, 20_000} {
		b.Run(fmt.Sprintf("parked=%d", n), func(b *testing.B) {
			state, next, ab := parkedTailFixture(b, n)
			doc := loadParked(b, state, n)
			writer := New(WithClientID(2))
			for _, u := range [][]byte{ab, state, next} {
				if err := ApplyUpdateV1(writer, u, nil); err != nil {
					b.Fatal(err)
				}
			}
			if err := ApplyUpdateV1(doc, next, nil); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				b.StopTimer()
				sv := writer.StateVector()
				writer.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, n+3+i, "x", nil) })
				u := EncodeStateAsUpdateV1(writer, sv)
				b.StartTimer()
				if err := ApplyUpdateV1(doc, u, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPendingParkOutOfOrder times parking one struct ahead of n parked
// structs of its client that arrived in descending clock order, each behind a
// gap: the cover inserts the struct's span before every span it holds.
func BenchmarkPendingParkOutOfOrder(b *testing.B) {
	single := func(clock uint64) []byte {
		it := &Item{ID: ID{Client: 2, Clock: clock}, Origin: &ID{Client: 9, Clock: 0}, Content: NewContentDeleted(1)}
		return encodeStructStoreV1(map[ClientID][]*Item{2: {it}}, newDeleteSet(), nil, newStructStore())
	}
	for _, n := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("parked=%d", n), func(b *testing.B) {
			fill := func() *Doc {
				doc := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
				for i := n; i > 0; i-- {
					if err := ApplyUpdateV1(doc, single(uint64(4*i+2)), nil); err != nil {
						b.Fatal(err)
					}
				}
				return doc
			}
			doc := fill()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				b.StopTimer()
				if i > 0 && i%n == 0 {
					doc = fill()
				}
				u := single(uint64(4 * (n - i%n)))
				b.StartTimer()
				if err := ApplyUpdateV1(doc, u, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPendingParkBatch times one update parking k structs of client 2
// between the s spans the queue holds for client 2 (out of order, each behind
// a gap): addCover's insertion below coverInsertWork, its merge above.
func BenchmarkPendingParkBatch(b *testing.B) {
	item := func(clock uint64) *Item {
		return &Item{ID: ID{Client: 2, Clock: clock}, Origin: &ID{Client: 9, Clock: 0}, Content: NewContentDeleted(1)}
	}
	enc := func(items []*Item) []byte {
		return encodeStructStoreV1(map[ClientID][]*Item{2: items}, newDeleteSet(), nil, newStructStore())
	}
	for _, sk := range [][2]int{{1_000, 32}, {10_000, 9}, {10_000, 52}, {10_000, 53}, {10_000, 1_000}, {100_000, 2}, {100_000, 5}, {100_000, 6}, {100_000, 32}} {
		s, k := sk[0], sk[1]
		b.Run(fmt.Sprintf("spans=%d/batch=%d", s, k), func(b *testing.B) {
			var batch []*Item
			for i := 1; i <= k; i++ {
				batch = append(batch, item(uint64(4*i)))
			}
			u := enc(batch)
			fill := func() *Doc {
				doc := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
				for i := s; i > 0; i-- {
					if err := ApplyUpdateV1(doc, enc([]*Item{item(uint64(4*i + 2))}), nil); err != nil {
						b.Fatal(err)
					}
				}
				doc.store.pending.coverOf()
				return doc
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				doc := fill()
				b.StartTimer()
				if err := ApplyUpdateV1(doc, u, nil); err != nil {
					b.Fatal(err)
				}
				if got := doc.PendingStats().Items; got != s+k {
					b.Fatalf("%d parked, want %d", got, s+k)
				}
			}
		})
	}
}

// BenchmarkPendingParkGCBatch: the queue holds client 2's clocks [2, 2+2s) as
// alternating GC structs and items, so its span list is one span and its GC
// span list is s spans. One update then parks k GC copies of the item clocks:
// each joins two GC spans.
func BenchmarkPendingParkGCBatch(b *testing.B) {
	gc := func(clock uint64) *Item {
		return &Item{ID: ID{Client: 2, Clock: clock}, Content: NewContentDeleted(1), Deleted: true}
	}
	item := func(clock uint64) *Item {
		return &Item{ID: ID{Client: 2, Clock: clock}, Origin: &ID{Client: 9, Clock: 0}, Content: NewContentDeleted(1)}
	}
	enc := func(items []*Item) []byte {
		return encodeStructStoreV1(map[ClientID][]*Item{2: items}, newDeleteSet(), nil, newStructStore())
	}
	for _, s := range []int{10_000, 25_000} {
		k := s - 1
		b.Run(fmt.Sprintf("gcspans=%d/batch=%d", s, k), func(b *testing.B) {
			fill, batch := make([]*Item, 0, 2*s), make([]*Item, 0, k)
			for i := range s {
				fill = append(fill, gc(uint64(2+2*i)), item(uint64(3+2*i)))
			}
			for i := range k {
				batch = append(batch, gc(uint64(3+2*i)))
			}
			state, u := enc(fill), enc(batch)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				doc := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
				if err := ApplyUpdateV1(doc, state, nil); err != nil {
					b.Fatal(err)
				}
				c := doc.store.pending.coverOf()
				if len(c.spans[2]) != 1 || len(c.gc[2]) != s {
					b.Fatalf("spans %d, gc spans %d", len(c.spans[2]), len(c.gc[2]))
				}
				b.StartTimer()
				if err := ApplyUpdateV1(doc, u, nil); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if got := doc.PendingStats().Items; got != 2*s+k {
					b.Fatalf("%d parked, want %d", got, 2*s+k)
				}
				b.StartTimer()
			}
		})
	}
}
