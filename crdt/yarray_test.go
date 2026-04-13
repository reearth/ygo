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

	assert.Equal(t, []any{int64(1), int64(10), int64(2), int64(3)}, arr.ToSlice())
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

	assert.Equal(t, []any{int64(1), int64(4), int64(5)}, arr.ToSlice())
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
	assert.Equal(t, int64(1), got[0])
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

	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, arr.Slice(1, 4))
	assert.Equal(t, []any{int64(0), int64(1), int64(2), int64(3), int64(4)}, arr.Slice(0, 5))
	assert.Equal(t, []any{int64(0), int64(1), int64(2), int64(3), int64(4)}, arr.Slice(0, 99)) // clamps to Len()
	assert.Equal(t, []any{int64(4)}, arr.Slice(4, 5))
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
	assert.Equal(t, []any{int64(10), int64(30)}, result)
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

// ── YArray.Move tests ─────────────────────────────────────────────────────────

func TestUnit_YArray_Move_ForwardByOne(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 1) }) // move "a" after "b"
	assert.Equal(t, []any{"b", "a", "c"}, arr.ToSlice())
}

func TestUnit_YArray_Move_BackwardByOne(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 2, 1) }) // move "c" before "b"
	assert.Equal(t, []any{"a", "c", "b"}, arr.ToSlice())
}

func TestUnit_YArray_Move_ToStart(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 2, 0) }) // move "c" to front
	assert.Equal(t, []any{"c", "a", "b"}, arr.ToSlice())
}

func TestUnit_YArray_Move_ToEnd(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 3) }) // move "a" to end
	assert.Equal(t, []any{"b", "c", "a"}, arr.ToSlice())
}

func TestUnit_YArray_Move_NoopSameIndex(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c"}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 1, 1) }) // no-op
	assert.Equal(t, []any{"a", "b", "c"}, arr.ToSlice())
}

func TestUnit_YArray_Slice_StartGreaterThanEnd_ReturnsNil(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{1, 2, 3}) })

	assert.Nil(t, arr.Slice(5, 2))            // start > end
	assert.Nil(t, arr.Slice(10, 0))           // start >> end after clamp
	assert.Equal(t, []any{}, arr.Slice(0, 0)) // empty range, not nil
}

// ── CRDT-safe Move convergence ────────────────────────────────────────────────

// TestInteg_YArray_Move_TwoPeer_DifferentItems checks that two peers each
// moving a DIFFERENT element converge to the same state.
//
// Both moves send their element to the END of the array — past all existing
// items. This avoids the YATA ordering constraint (lower ClientID goes first)
// that arises when a higher-ClientID peer tries to insert before the lower
// ClientID peer's items. The result is deterministic: both ContentMove items
// share the same origin (the last item from client1) and no OriginRight, so
// YATA places them in ClientID order: ContentMove1 (client1) then ContentMove2
// (client2). The element that was moved to the end by client1 ("a") renders
// before the element moved by client2 ("b").
func TestInteg_YArray_Move_TwoPeer_DifferentItems(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)

	arr1 := doc1.GetArray("list")
	arr2 := doc2.GetArray("list")

	// Both start from the same initial state pushed by doc1.
	doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{"a", "b", "c"}) })
	update0 := EncodeStateAsUpdateV1(doc1, nil)
	require.NoError(t, ApplyUpdateV1(doc2, update0, nil))

	// doc1 moves "a" (index 0) to end (index 2) → local result [b, c, a]
	doc1.Transact(func(txn *Transaction) { arr1.Move(txn, 0, 2) })
	assert.Equal(t, []any{"b", "c", "a"}, arr1.ToSlice(), "doc1 local result")

	// doc2 moves "b" (index 1) to end (index 2) → local result [a, c, b]
	doc2.Transact(func(txn *Transaction) { arr2.Move(txn, 1, 2) })
	assert.Equal(t, []any{"a", "c", "b"}, arr2.ToSlice(), "doc2 local result")

	// Exchange delta updates.
	sv1Before := doc1.store.StateVector()
	sv2Before := doc2.store.StateVector()

	update1to2 := EncodeStateAsUpdateV1(doc1, sv2Before)
	update2to1 := EncodeStateAsUpdateV1(doc2, sv1Before)

	require.NoError(t, ApplyUpdateV1(doc2, update1to2, nil))
	require.NoError(t, ApplyUpdateV1(doc1, update2to1, nil))

	// Both documents must converge to the same slice.
	s1 := arr1.ToSlice()
	s2 := arr2.ToSlice()
	assert.Equal(t, s1, s2, "docs must converge")
	assert.Len(t, s1, 3, "no elements lost")
	// "c" (not moved) should appear first; "a" and "b" at the end in ClientID order.
	assert.Equal(t, "c", s1[0], "'c' (unmoved) is first")
	assert.Equal(t, "a", s1[1], "'a' (moved by client1) before 'b' (moved by client2)")
	assert.Equal(t, "b", s1[2], "'b' (moved by client2) last")
}

// TestInteg_YArray_Move_TwoPeer_SameItem checks that two peers concurrently
// moving THE SAME element converge: the peer with the lower ClientID wins, and
// the element appears exactly once.
func TestInteg_YArray_Move_TwoPeer_SameItem(t *testing.T) {
	doc1 := newTestDoc(1) // lower ClientID → wins the race
	doc2 := newTestDoc(2)

	arr1 := doc1.GetArray("list")
	arr2 := doc2.GetArray("list")

	// Shared initial state from doc1.
	doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{"a", "b", "c"}) })
	update0 := EncodeStateAsUpdateV1(doc1, nil)
	require.NoError(t, ApplyUpdateV1(doc2, update0, nil))

	// Both peers move element "a" (index 0) but to different destinations.
	// doc1 moves "a" to index 1 (→ [b, a, c]); doc2 moves "a" to index 2 (→ [b, c, a]).
	doc1.Transact(func(txn *Transaction) { arr1.Move(txn, 0, 1) })
	doc2.Transact(func(txn *Transaction) { arr2.Move(txn, 0, 2) })

	// Exchange delta updates.
	sv1Before := doc1.store.StateVector()
	sv2Before := doc2.store.StateVector()

	update1to2 := EncodeStateAsUpdateV1(doc1, sv2Before)
	update2to1 := EncodeStateAsUpdateV1(doc2, sv1Before)

	require.NoError(t, ApplyUpdateV1(doc2, update1to2, nil))
	require.NoError(t, ApplyUpdateV1(doc1, update2to1, nil))

	s1 := arr1.ToSlice()
	s2 := arr2.ToSlice()

	// Must converge.
	assert.Equal(t, s1, s2, "docs must converge")
	// Element "a" must appear exactly once (no duplicates, no losses).
	assert.Len(t, s1, 3, "length must be preserved")
	aCount := 0
	for _, v := range s1 {
		if v == "a" {
			aCount++
		}
	}
	assert.Equal(t, 1, aCount, "element 'a' must appear exactly once")
	// doc1 (ClientID=1) wins: "a" appears at the position doc1 chose (index 1).
	assert.Equal(t, "a", s1[1], "lower ClientID (doc1) wins: 'a' at index 1")
}

func TestUnit_YArray_Get_NestedYMap_ReturnsType(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	nested := doc.GetMap("nested")
	doc.Transact(func(txn *Transaction) {
		nested.Set(txn, "key", "val")
		arr.Push(txn, []any{nested})
	})
	got := arr.Get(0)
	require.NotNil(t, got, "Get() must return nested type, not nil")
	_, ok := got.(*YMap)
	assert.True(t, ok, "should be *YMap")
}
