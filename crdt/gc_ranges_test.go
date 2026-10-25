package crdt

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_GCTransaction_OverlappingRanges(t *testing.T) {
	doc := newTestDoc(1)
	defer doc.Destroy()
	txn := newTxn(doc)
	for i := 0; i < 6; i++ {
		doc.store.Append(&Item{ID: ID{Client: 1, Clock: uint64(i * 4)}, Content: NewContentString("abcd"), Deleted: i != 2})
	}
	// Includes a partial item, unsorted and overlapping ranges, and a live item.
	txn.deleteSet.clients[1] = []DeleteRange{{Clock: 19, Len: 1}, {Clock: 3, Len: 7}, {Clock: 5, Len: 1}, {Clock: 24, Len: 5}}
	txn.deleteSet.clients[2] = []DeleteRange{{Clock: 0, Len: 1}}
	gcTxnDeleteSet(doc, txn)
	for i, item := range doc.store.clients[1] {
		_, gc := item.Content.(*ContentDeleted)
		require.Equal(t, i == 0 || i == 1 || i == 4, gc, "item %d", i)
		require.Equal(t, 4, item.Content.Len())
	}
}

func TestUnit_GCTransaction_RangeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deleteRange DeleteRange
		collected   []bool
	}{
		{"before_first_no_overlap", DeleteRange{Clock: 0, Len: 4}, []bool{false, false, false}},
		{"before_first_partial_overlap", DeleteRange{Clock: 0, Len: 5}, []bool{true, false, false}},
		{"inside_gap", DeleteRange{Clock: 9, Len: 2}, []bool{false, false, false}},
		{"gap_to_partial_overlap", DeleteRange{Clock: 10, Len: 3}, []bool{false, true, false}},
		{"exact_item_boundary", DeleteRange{Clock: 12, Len: 4}, []bool{false, true, false}},
		{"after_last", DeleteRange{Clock: 20, Len: 5}, []bool{false, false, false}},
		{"zero_length_at_boundary", DeleteRange{Clock: 12, Len: 0}, []bool{false, false, false}},
		// Preserve the old scan's treatment of a zero-length range inside an item.
		{"zero_length_inside_item", DeleteRange{Clock: 5, Len: 0}, []bool{true, false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			defer doc.Destroy()
			txn := newTxn(doc)
			for _, clock := range []uint64{4, 12, 16} {
				doc.store.Append(&Item{ID: ID{Client: 1, Clock: clock}, Content: NewContentString("abcd"), Deleted: true})
			}
			txn.deleteSet.clients[1] = []DeleteRange{tc.deleteRange}
			gcTxnDeleteSet(doc, txn)
			for i, item := range doc.store.clients[1] {
				_, collected := item.Content.(*ContentDeleted)
				require.Equal(t, tc.collected[i], collected, "item %d", i)
				require.Equal(t, 4, item.Content.Len(), "item %d", i)
			}
		})
	}
}

// gcRangeCountingContent detects prefix scans without timing-sensitive assertions.
type gcRangeCountingContent struct {
	Content
	lenCalls int
}

func (c *gcRangeCountingContent) Len() int {
	c.lenCalls++
	return c.Content.Len()
}

func TestUnit_GCTransaction_SeekSkipsPrefix(t *testing.T) {
	doc := newTestDoc(1)
	defer doc.Destroy()
	txn := newTxn(doc)
	prefix := &gcRangeCountingContent{Content: NewContentString("abcd")}
	for i := 0; i < 128; i++ {
		doc.store.Append(&Item{ID: ID{Client: 1, Clock: uint64(i * 4)}, Content: prefix, Deleted: true})
	}
	target := &Item{ID: ID{Client: 1, Clock: 512}, Content: NewContentString("abcd"), Deleted: true}
	doc.store.Append(target)
	txn.deleteSet.clients[1] = []DeleteRange{{Clock: 513, Len: 1}}
	gcTxnDeleteSet(doc, txn)
	require.Zero(t, prefix.lenCalls, "seek must not inspect content preceding the range")
	require.IsType(t, &ContentDeleted{}, target.Content)
	require.Equal(t, 4, target.Content.Len())
}

func TestUnit_GCTransaction_MatchesLinearScan(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			doc, reference := newTestDoc(1), newTestDoc(1)
			defer doc.Destroy()
			defer reference.Destroy()
			txn, referenceTxn := newTxn(doc), newTxn(reference)
			for client := ClientID(1); client <= 3; client++ {
				clock := uint64(rng.Intn(8))
				// Items remain sorted and non-overlapping, as required by StructStore.
				for n := rng.Intn(65); n > 0; n-- {
					content := Content(NewContentString(strings.Repeat("a😀", 1+rng.Intn(8))))
					if rng.Intn(4) == 0 {
						content = NewContentDeleted(content.Len())
					}
					item := &Item{ID: ID{Client: client, Clock: clock}, Content: content, Deleted: rng.Intn(4) != 0}
					doc.store.Append(item)
					reference.store.Append(&Item{ID: item.ID, Content: content.Copy(), Deleted: item.Deleted})
					clock += uint64(content.Len() + rng.Intn(8))
				}
				// Leave ranges unsorted and overlapping; include empty ranges and
				// starts before, inside and after the stored clock interval.
				ranges := make([]DeleteRange, 40)
				for i := range ranges {
					ranges[i] = DeleteRange{Clock: uint64(rng.Intn(int(clock) + 20)), Len: uint64(rng.Intn(50))}
				}
				txn.deleteSet.clients[client] = ranges
				referenceTxn.deleteSet.clients[client] = append([]DeleteRange(nil), ranges...)
			}
			// Also exercise a client that has no stored items.
			txn.deleteSet.clients[4] = []DeleteRange{{Clock: 0, Len: 10}}
			referenceTxn.deleteSet.clients[4] = []DeleteRange{{Clock: 0, Len: 10}}
			gcTxnDeleteSetLinearScan(reference, referenceTxn)
			gcTxnDeleteSet(doc, txn)
			require.Equal(t, reference.store.clients, doc.store.clients)
			// Repeating GC must preserve the result, including collected lengths.
			gcTxnDeleteSet(doc, txn)
			require.Equal(t, reference.store.clients, doc.store.clients)
		})
	}
}

// gcTxnDeleteSetLinearScan preserves the pre-#258 implementation as a test oracle.
func gcTxnDeleteSetLinearScan(doc *Doc, txn *Transaction) {
	for client, ranges := range txn.deleteSet.clients {
		items := doc.store.clients[client]
		for _, r := range ranges {
			rangeEnd := r.Clock + r.Len
			for _, item := range items {
				if item.ID.Clock >= rangeEnd {
					break
				}
				if item.ID.Clock+uint64(item.Content.Len()) <= r.Clock {
					continue
				}
				if !item.Deleted {
					continue
				}
				if _, alreadyGC := item.Content.(*ContentDeleted); alreadyGC {
					continue
				}
				item.Content = NewContentDeleted(item.Content.Len())
			}
		}
	}
}

func BenchmarkGCTransactionSparseRanges(b *testing.B) {
	for _, size := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			doc := newTestDoc(1)
			defer doc.Destroy()
			txn := newTxn(doc)
			for i := 0; i < size; i++ {
				doc.store.Append(&Item{ID: ID{Client: 1, Clock: uint64(i)}, Content: NewContentDeleted(1), Deleted: true})
				if i%10 == 0 {
					txn.deleteSet.clients[1] = append(txn.deleteSet.clients[1], DeleteRange{Clock: uint64(i), Len: 1})
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				gcTxnDeleteSet(doc, txn)
			}
		})
	}
}
