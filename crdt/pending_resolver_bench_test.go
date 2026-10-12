//go:build benchheavy

package crdt

import (
	"fmt"
	"testing"
)

func pendingLinearClientQueue(version, n int, complete bool) []byte {
	source := New()
	defer source.Destroy()
	root := source.GetMap("root")
	key := "child"
	parentClient := ClientID(n + 100)
	if version == 2 {
		parentClient = 1
	}
	parent := &Item{ID: ID{Client: parentClient}, Parent: &root.abstractType, ParentSub: &key, Content: NewContentType(&NewMapPrelim().abstractType)}
	groups := make(map[ClientID][]*Item, n+1)
	for i := 0; i < n; i++ {
		name := fmt.Sprint(i)
		id := ClientID(i + 100)
		groups[id] = []*Item{{ID: ID{Client: id}, parentID: &parent.ID, ParentSub: &name, Content: NewContentAny(i)}}
	}
	if complete {
		groups[parentClient] = []*Item{parent}
	}
	encode := encodeStructStoreV1
	if version == 2 {
		encode = encodeStructStoreV2
	}
	return encode(groups, newDeleteSet(), nil, source.store)
}

func BenchmarkPendingLinearClientQueue(b *testing.B) {
	const n = 20000
	for _, version := range []int{1, 2} {
		for _, complete := range []bool{true, false} {
			data := pendingLinearClientQueue(version, n, complete)
			b.Run(fmt.Sprintf("V%d/complete=%t", version, complete), func(b *testing.B) {
				apply := ApplyUpdateV1
				if version == 2 {
					apply = ApplyUpdateV2
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					doc := New(WithMaxPendingItems(n + 1))
					b.StartTimer()
					err := apply(doc, data, nil)
					b.StopTimer()
					if err != nil {
						b.Fatal(err)
					}
					if !complete {
						if doc.PendingStats().Items != n {
							b.Fatal("missing queue changed")
						}
					} else {
						val, ok := doc.GetMap("root").Get("child")
						if !ok || len(val.(*YMap).Entries()) != n || doc.PendingStats().Items != 0 {
							b.Fatal("complete queue changed")
						}
					}
					doc.Destroy()
					b.StartTimer()
				}
			})
		}
	}
}

func BenchmarkPendingMultiStructChain(b *testing.B) {
	for _, version := range []int{1, 2} {
		for _, n := range []int{1000, 20000} {
			data := resolverMultiStructUpdate(version, n)
			b.Run(fmt.Sprintf("V%d/n=%d/cap=16", version, n), func(b *testing.B) {
				apply := ApplyUpdateV1
				if version == 2 {
					apply = ApplyUpdateV2
				}
				b.ReportAllocs()
				b.ResetTimer()
				rejected := 0
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					doc := New(WithMaxPendingItems(16))
					b.StartTimer()
					err := apply(doc, data, nil)
					b.StopTimer()
					if err != nil {
						rejected++
					} else {
						if doc.GetText("text").Len() != 2*n || len(doc.StateVector()) != n || doc.PendingStats().Items != 0 {
							b.Fatal("incomplete restore")
						}
						for _, clock := range doc.StateVector() {
							if clock != 2 {
								b.Fatal("client clock")
							}
						}
					}
					doc.Destroy()
					b.StartTimer()
				}
				b.ReportMetric(float64(rejected)/float64(b.N), "rejections/op")
			})
		}
	}
}

// Overlapping short ranges cannot cover the predecessor clock in the long
// ranges. The coverage index must still select the longer producer.
func resolverOverlapQueue(doc *Doc, n int) []*Item {
	text := doc.GetText("text")
	pending := make([]*Item, 0, 2*n)
	for i := 0; i < n; i++ {
		c := ClientID(i + 1)
		var origin *ID
		if i+1 < n {
			origin = &ID{Client: c + 1, Clock: 2}
		}
		var parent *abstractType
		if origin == nil {
			parent = &text.abstractType
		}
		pending = append(pending,
			&Item{ID: ID{Client: c}, Parent: parent, Origin: origin, Content: NewContentString("abc")},
			&Item{ID: ID{Client: c, Clock: 1}, Parent: parent, Origin: origin, Content: NewContentString("b")})
	}
	return pending
}

func BenchmarkPendingOverlapFallback(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				doc := New(WithMaxPendingItems(2*n + 1))
				pending := resolverOverlapQueue(doc, n)
				b.StartTimer()
				var err error
				doc.Transact(func(txn *Transaction) { err = resolveWithinUpdatePending(txn, pending) })
				b.StopTimer()
				if err != nil || doc.PendingStats().Items != 0 || len(doc.StateVector()) != n || doc.GetText("text").Len() != 3*n {
					b.Fatalf("invalid fallback: %v", err)
				}
				doc.Destroy()
				b.StartTimer()
			}
		})
	}
}

// Compare the same complete chain using the fixed-point reference and the
// retry path directly, without enabling the dependency scheduler.
func BenchmarkPendingFixedPointFallback(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		for _, reference := range []bool{true, false} {
			b.Run(fmt.Sprintf("n=%d/reference=%t", n, reference), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					doc := New(WithMaxPendingItems(n + 1))
					text := doc.GetText("text")
					pending := make([]*Item, n)
					for j := 0; j < n; j++ {
						c := ClientID(j + 1)
						var origin *ID
						if j+1 < n {
							origin = &ID{Client: c + 1}
						}
						var parent *abstractType
						if origin == nil {
							parent = &text.abstractType
						}
						pending[j] = &Item{ID: ID{Client: c}, Parent: parent, Origin: origin, Content: NewContentString("x")}
					}
					b.StartTimer()
					var err error
					doc.Transact(func(txn *Transaction) {
						if reference {
							err = referenceWithinUpdatePending(txn, pending)
							return
						}
						for len(pending) > 0 {
							remaining := retryWithinUpdatePending(txn, pending)
							if len(remaining) == len(pending) {
								err = parkWithinUpdatePending(txn, remaining)
								return
							}
							pending = remaining
						}
					})
					b.StopTimer()
					if err != nil || doc.PendingStats().Items != 0 || text.Len() != n {
						b.Fatalf("incomplete fixed point: %v", err)
					}
					doc.Destroy()
					b.StartTimer()
				}
			})
		}
	}
}
