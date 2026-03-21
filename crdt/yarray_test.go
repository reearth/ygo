package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── YArray ────────────────────────────────────────────────────────────────────

func TestUnit_YArray_Push_And_Len(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{1, 2, 3})
	})

	assert.Equal(t, 3, arr.Len())
}

func TestUnit_YArray_Get(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"a", "b", "c"})
	})

	assert.Equal(t, "a", arr.Get(0))
	assert.Equal(t, "b", arr.Get(1))
	assert.Equal(t, "c", arr.Get(2))
	assert.Nil(t, arr.Get(3))
}

func TestUnit_YArray_Insert_AtStart(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"b", "c"})
		arr.Insert(txn, 0, []any{"a"})
	})

	assert.Equal(t, []any{"a", "b", "c"}, arr.ToSlice())
}

func TestUnit_YArray_Insert_InMiddle(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{1, 2, 3})
		arr.Insert(txn, 1, []any{10})
	})

	assert.Equal(t, []any{1, 10, 2, 3}, arr.ToSlice())
}

func TestUnit_YArray_Delete(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{1, 2, 3, 4, 5})
	})
	doc.Transact(func(txn *Transaction) {
		arr.Delete(txn, 1, 2) // remove 2 and 3
	})

	assert.Equal(t, []any{1, 4, 5}, arr.ToSlice())
	assert.Equal(t, 3, arr.Len())
}

func TestUnit_YArray_Delete_EntireArray(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"x", "y"})
	})
	doc.Transact(func(txn *Transaction) {
		arr.Delete(txn, 0, arr.Len())
	})

	assert.Equal(t, 0, arr.Len())
	assert.Empty(t, arr.ToSlice())
}

func TestUnit_YArray_ToSlice_MixedTypes(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{1, "two", true, nil})
	})

	got := arr.ToSlice()
	require.Len(t, got, 4)
	assert.Equal(t, 1, got[0])
	assert.Equal(t, "two", got[1])
	assert.Equal(t, true, got[2])
	assert.Nil(t, got[3])
}

func TestUnit_YArray_Observe_FiresOnce(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	calls := 0
	arr.Observe(func(e YArrayEvent) { calls++ })

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{1})
		arr.Push(txn, []any{2})
	})

	assert.Equal(t, 1, calls, "observer must fire once per transaction, not per operation")
}

func TestUnit_YArray_Observe_Unsubscribe(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	calls := 0
	unsub := arr.Observe(func(_ YArrayEvent) { calls++ })

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{1}) })
	unsub()
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{2}) })

	assert.Equal(t, 1, calls)
}

func TestUnit_YArray_Slice(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{0, 1, 2, 3, 4})
	})

	assert.Equal(t, []any{1, 2, 3}, arr.Slice(1, 4))
	assert.Equal(t, []any{0, 1, 2, 3, 4}, arr.Slice(0, 5))
	assert.Equal(t, []any{0, 1, 2, 3, 4}, arr.Slice(0, 99)) // clamps to Len()
	assert.Equal(t, []any{4}, arr.Slice(4, 5))
	assert.Empty(t, arr.Slice(2, 2)) // empty range
}

func TestUnit_YArray_ForEach(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"a", "b", "c"})
	})

	var indices []int
	var vals []any
	arr.ForEach(func(i int, v any) {
		indices = append(indices, i)
		vals = append(vals, v)
	})

	assert.Equal(t, []int{0, 1, 2}, indices)
	assert.Equal(t, []any{"a", "b", "c"}, vals)
}

func TestUnit_YArray_ForEach_SkipsDeleted(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{10, 20, 30}) })
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 1, 1) }) // remove 20

	var result []any
	arr.ForEach(func(_ int, v any) { result = append(result, v) })
	assert.Equal(t, []any{10, 30}, result)
}

// ── YArray integration ────────────────────────────────────────────────────────

func TestInteg_YArray_TwoPeer_Convergence(t *testing.T) {
	// doc1 appends [1,2], doc2 appends [3,4] concurrently; both must converge.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)

	arr1 := doc1.GetArray("list")
	arr2 := doc2.GetArray("list")

	// Each peer inserts locally.
	doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{1, 2}) })
	doc2.Transact(func(txn *Transaction) { arr2.Push(txn, []any{3, 4}) })

	// Cross-apply: simulate exchanging items by integrating them directly.
	// We re-create items from doc1 in doc2 and vice-versa.
	applyItemsTo := func(src *Doc, dst *Doc, dstArr *YArray) {
		dst.Transact(func(txn *Transaction) {
			src.store.IterateFrom(dst.store.StateVector(), func(item *Item) {
				clone := &Item{
					ID:          item.ID,
					Origin:      item.Origin,
					OriginRight: item.OriginRight,
					Left:        dst.store.Find(item.ID), // placeholder; integrate will fix
					Parent:      &dstArr.abstractType,
					Content:     item.Content.Copy(),
					Deleted:     false,
				}
				// Resolve Left from origin.
				if item.Origin != nil {
					clone.Left = dst.store.Find(*item.Origin)
				} else {
					clone.Left = nil
				}
				clone.integrate(txn, 0)
			})
		})
	}

	applyItemsTo(doc1, doc2, arr2)
	applyItemsTo(doc2, doc1, arr1)

	assert.Equal(t, arr1.ToSlice(), arr2.ToSlice(), "arrays must converge")
}

func TestInteg_YArray_SequentialInserts_Converge(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")

	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"a"})
		arr.Push(txn, []any{"b"})
		arr.Push(txn, []any{"c"})
	})

	assert.Equal(t, []any{"a", "b", "c"}, arr.ToSlice())
}
