package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// ── Unit tests ────────────────────────────────────────────────────────────────

func TestUnit_UpdateV1_RoundTrip_EmptyDoc(t *testing.T) {
	doc := newTestDoc(1)
	update := EncodeStateAsUpdateV1(doc, nil)

	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil))

	assert.Equal(t, doc.StateVector(), doc2.StateVector())
}

func TestUnit_UpdateV1_RoundTrip_TextInsert(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "Hello World", nil) })

	update := EncodeStateAsUpdateV1(doc, nil)

	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil))

	assert.Equal(t, "Hello World", doc2.GetText("content").ToString())
	assert.Equal(t, 11, doc2.GetText("content").Len())
}

func TestUnit_UpdateV1_RoundTrip_ArrayInsert(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("list")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{1, "two", true}) })

	update := EncodeStateAsUpdateV1(doc, nil)

	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil))

	assert.Equal(t, []any{int64(1), "two", true}, doc2.GetArray("list").ToSlice())
}

func TestUnit_UpdateV1_RoundTrip_MapSet(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "key", "value")
		m.Set(txn, "num", 42)
	})

	update := EncodeStateAsUpdateV1(doc, nil)

	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil))

	v1, _ := doc2.GetMap("m").Get("key")
	v2, _ := doc2.GetMap("m").Get("num")
	assert.Equal(t, "value", v1)
	assert.Equal(t, int64(42), v2)
}

func TestUnit_UpdateV1_RoundTrip_WithDeletes(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "Hello World", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 5, 6) }) // delete " World"

	update := EncodeStateAsUpdateV1(doc, nil)

	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil))

	assert.Equal(t, "Hello", doc2.GetText("content").ToString())
}

func TestUnit_UpdateV1_MultipleTypes(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("txt")
	arr := doc.GetArray("arr")
	mp := doc.GetMap("mp")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		arr.Push(txn, []any{1, 2, 3})
		mp.Set(txn, "x", 99)
	})

	update := EncodeStateAsUpdateV1(doc, nil)

	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil))

	assert.Equal(t, "hello", doc2.GetText("txt").ToString())
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, doc2.GetArray("arr").ToSlice())
	vx, _ := doc2.GetMap("mp").Get("x")
	assert.Equal(t, int64(99), vx)
}

func TestUnit_UpdateV2_SmallerThanV1(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")
	// Insert many individual characters so the column-oriented V2 format can
	// compress repeated client IDs and info bytes via RLE.
	for i := 0; i < 100; i++ {
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, txt.Len(), "a", nil)
		})
	}

	v1 := EncodeStateAsUpdateV1(doc, nil)
	v2 := EncodeStateAsUpdateV2(doc, nil)

	assert.Less(t, len(v2), len(v1), "V2 column-oriented format should be smaller than V1 for many items")
}

func TestUnit_V1toV2_Roundtrip(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "Hello", nil) })

	v1 := EncodeStateAsUpdateV1(doc, nil)
	v2, err := UpdateV1ToV2(v1)
	require.NoError(t, err)

	v1Back, err := UpdateV2ToV1(v2)
	require.NoError(t, err)

	assert.Equal(t, v1, v1Back)
}

func TestUnit_ApplyUpdateV2_RoundTrip(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello v2", nil) })

	v2 := EncodeStateAsUpdateV2(doc, nil)

	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))

	assert.Equal(t, "hello v2", doc2.GetText("t").ToString())
}

func TestUnit_ApplyUpdate_Idempotent(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "Hello", nil) })

	update := EncodeStateAsUpdateV1(doc, nil)

	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil))
	require.NoError(t, ApplyUpdateV1(doc2, update, nil)) // apply twice — must be a no-op

	assert.Equal(t, "Hello", doc2.GetText("t").ToString())
	assert.Equal(t, 5, doc2.GetText("t").Len())
}

func TestUnit_MergeUpdates_OrderIndependent(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "Alice", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "Bob", nil) })

	u1 := EncodeStateAsUpdateV1(doc1, nil)
	u2 := EncodeStateAsUpdateV1(doc2, nil)

	merged12, err := MergeUpdatesV1(u1, u2)
	require.NoError(t, err)
	merged21, err := MergeUpdatesV1(u2, u1)
	require.NoError(t, err)

	// Both orderings must produce the same result.
	docA := New(WithClientID(3))
	docB := New(WithClientID(4))
	require.NoError(t, ApplyUpdateV1(docA, merged12, nil))
	require.NoError(t, ApplyUpdateV1(docB, merged21, nil))

	assert.Equal(t, docA.GetText("t").ToString(), docB.GetText("t").ToString())
}

func TestUnit_DiffUpdate_OnlyMissing(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "Hello", nil) })
	svAfterHello := doc.StateVector()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 5, " World", nil) })

	fullUpdate := EncodeStateAsUpdateV1(doc, nil)

	diff, err := DiffUpdateV1(fullUpdate, svAfterHello)
	require.NoError(t, err)

	// The diff should be strictly smaller than the full update.
	assert.Less(t, len(diff), len(fullUpdate))

	// Applying the full update + diff idempotently must give the correct result.
	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, fullUpdate, nil))
	require.NoError(t, ApplyUpdateV1(doc2, diff, nil)) // idempotent
	assert.Equal(t, "Hello World", doc2.GetText("t").ToString())
}

func TestUnit_ApplyUpdate_OutOfOrder(t *testing.T) {
	// Two independent clients — apply updates in both orders and check convergence.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "A", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "B", nil) })

	u1 := EncodeStateAsUpdateV1(doc1, nil)
	u2 := EncodeStateAsUpdateV1(doc2, nil)

	docA := New(WithClientID(3))
	require.NoError(t, ApplyUpdateV1(docA, u1, nil))
	require.NoError(t, ApplyUpdateV1(docA, u2, nil))

	docB := New(WithClientID(4))
	require.NoError(t, ApplyUpdateV1(docB, u2, nil))
	require.NoError(t, ApplyUpdateV1(docB, u1, nil))

	assert.Equal(t, docA.GetText("t").ToString(), docB.GetText("t").ToString())
}

func TestUnit_EncodeStateVector_RoundTrip(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	encoded := EncodeStateVectorV1(doc)
	decoded, err := DecodeStateVectorV1(encoded)
	require.NoError(t, err)

	assert.Equal(t, doc.StateVector(), decoded)
}

func TestUnit_DocConvenienceMethods(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	update := doc.EncodeStateAsUpdate()

	doc2 := New(WithClientID(2))
	require.NoError(t, doc2.ApplyUpdate(update))
	assert.Equal(t, "hello", doc2.GetText("t").ToString())
}

// ── Integration tests ─────────────────────────────────────────────────────────

func TestInteg_Update_TwoPeer_TextConvergence(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "Alice", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "Bob", nil) })

	u1 := EncodeStateAsUpdateV1(doc1, nil)
	u2 := EncodeStateAsUpdateV1(doc2, nil)

	require.NoError(t, ApplyUpdateV1(doc1, u2, nil))
	require.NoError(t, ApplyUpdateV1(doc2, u1, nil))

	// Both peers converge; client 1 < 2 so "Alice" precedes "Bob".
	assert.Equal(t, doc1.GetText("t").ToString(), doc2.GetText("t").ToString())
	assert.Equal(t, "AliceBob", doc1.GetText("t").ToString())
}

func TestInteg_Update_TwoPeer_ArrayConvergence(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	arr1 := doc1.GetArray("a")
	arr2 := doc2.GetArray("a")

	doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{1, 2}) })
	doc2.Transact(func(txn *Transaction) { arr2.Push(txn, []any{3, 4}) })

	u1 := EncodeStateAsUpdateV1(doc1, nil)
	u2 := EncodeStateAsUpdateV1(doc2, nil)

	require.NoError(t, ApplyUpdateV1(doc1, u2, nil))
	require.NoError(t, ApplyUpdateV1(doc2, u1, nil))

	assert.Equal(t, doc1.GetArray("a").ToSlice(), doc2.GetArray("a").ToSlice())
}

func TestInteg_Update_IncrementalSync(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	txt1 := doc1.GetText("t")

	// Step 1: doc1 inserts "Hello".
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "Hello", nil) })
	sv2 := doc2.StateVector() // doc2 is empty
	u := EncodeStateAsUpdateV1(doc1, sv2)
	require.NoError(t, ApplyUpdateV1(doc2, u, nil))
	assert.Equal(t, "Hello", doc2.GetText("t").ToString())

	// Step 2: doc1 appends " World".
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 5, " World", nil) })
	sv2 = doc2.StateVector()
	u = EncodeStateAsUpdateV1(doc1, sv2)
	require.NoError(t, ApplyUpdateV1(doc2, u, nil))
	assert.Equal(t, "Hello World", doc2.GetText("t").ToString())
}

func TestInteg_Update_DeleteSync(t *testing.T) {
	doc1 := newTestDoc(1)
	txt1 := doc1.GetText("t")
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "Hello World", nil) })
	doc1.Transact(func(txn *Transaction) { txt1.Delete(txn, 5, 6) })

	u := EncodeStateAsUpdateV1(doc1, nil)
	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, u, nil))

	assert.Equal(t, "Hello", doc2.GetText("t").ToString())
}

// ── Fuzz targets ──────────────────────────────────────────────────────────────

func fuzzSeedsV1() [][]byte {
	seeds := make([][]byte, 0, 6)

	// seed 1: simple text insert
	d1 := newTestDoc(1)
	t1 := d1.GetText("t")
	d1.Transact(func(txn *Transaction) { t1.Insert(txn, 0, "hello world", nil) })
	seeds = append(seeds, EncodeStateAsUpdateV1(d1, nil))

	// seed 2: insert + delete
	d2 := newTestDoc(2)
	t2 := d2.GetText("t")
	d2.Transact(func(txn *Transaction) { t2.Insert(txn, 0, "abcde", nil) })
	d2.Transact(func(txn *Transaction) { t2.Delete(txn, 1, 2) })
	seeds = append(seeds, EncodeStateAsUpdateV1(d2, nil))

	// seed 3: YMap
	d3 := newTestDoc(3)
	m3 := d3.GetMap("m")
	d3.Transact(func(txn *Transaction) {
		m3.Set(txn, "key", "value")
		m3.Set(txn, "num", 42)
	})
	seeds = append(seeds, EncodeStateAsUpdateV1(d3, nil))

	// seed 4: YArray with mixed types
	d4 := newTestDoc(4)
	a4 := d4.GetArray("a")
	d4.Transact(func(txn *Transaction) {
		a4.Insert(txn, 0, []any{"x", 1, true, nil})
	})
	seeds = append(seeds, EncodeStateAsUpdateV1(d4, nil))

	// seed 5: concurrent merge (two clients)
	d5a := newTestDoc(10)
	d5b := newTestDoc(20)
	t5a := d5a.GetText("t")
	t5b := d5b.GetText("t")
	d5a.Transact(func(txn *Transaction) { t5a.Insert(txn, 0, "Alice", nil) })
	d5b.Transact(func(txn *Transaction) { t5b.Insert(txn, 0, "Bob", nil) })
	u5a := EncodeStateAsUpdateV1(d5a, nil)
	u5b := EncodeStateAsUpdateV1(d5b, nil)
	merged, _ := MergeUpdatesV1(u5a, u5b)
	seeds = append(seeds, merged)

	// seed 6: empty
	seeds = append(seeds, []byte{})

	return seeds
}

func FuzzApplyUpdateV1(f *testing.F) {
	for _, s := range fuzzSeedsV1() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d := New()
		_ = ApplyUpdateV1(d, data, nil) // must not panic regardless of input
	})
}

func FuzzApplyUpdateV2(f *testing.F) {
	// Re-encode all V1 seeds as V2 for broader coverage.
	for _, s := range fuzzSeedsV1() {
		v2, err := UpdateV1ToV2(s)
		if err == nil {
			f.Add(v2)
		}
	}
	// A directly-encoded V2 seed.
	d := newTestDoc(1)
	tc := d.GetText("c")
	d.Transact(func(txn *Transaction) { tc.Insert(txn, 0, "hello", nil) })
	f.Add(EncodeStateAsUpdateV2(d, nil))

	f.Fuzz(func(t *testing.T, data []byte) {
		d := New()
		_ = ApplyUpdateV2(d, data, nil) // must not panic regardless of input
	})
}

// ── GC struct and cross-client tests ─────────────────────────────────────────

func TestUnit_ApplyUpdateV1_GCStructDecode(t *testing.T) {
	// Yjs encodes GC items as {info=0, VarUint(length)}. Verify the V1
	// decoder handles them without misaligning subsequent items.
	enc := encoding.NewEncoder()

	enc.WriteVarUint(1) // 1 client group
	enc.WriteVarUint(2) // 2 structs
	enc.WriteVarUint(1) // clientID = 1
	enc.WriteVarUint(0) // startClock = 0

	// Struct 0: GC (info=0, length=5)
	enc.WriteUint8(0)
	enc.WriteVarUint(5)

	// Struct 1: string item at clock 5, parent = root "content"
	enc.WriteUint8(wireString)  // tag=4, no flags
	enc.WriteUint8(1)           // parentInfo=1 → named root
	enc.WriteVarString("content")
	enc.WriteVarString("world")

	enc.WriteVarUint(0) // empty delete set

	doc := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))
	assert.Equal(t, "world", doc.GetText("content").ToString())
}

func TestUnit_ApplyUpdateV1_CrossClientParentResolution(t *testing.T) {
	// Client 200 creates "hello" in YText "t". Client 100 appends " world"
	// with origin={200,4}. Encoding order is by client ID: 100 before 200,
	// so client 100's origin is not yet decoded when first encountered.
	enc := encoding.NewEncoder()

	enc.WriteVarUint(2) // 2 client groups

	// Group 1: client=100
	enc.WriteVarUint(1)   // 1 struct
	enc.WriteVarUint(100) // clientID
	enc.WriteVarUint(0)   // startClock

	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(200) // origin client
	enc.WriteVarUint(4)   // origin clock (last char of "hello")
	enc.WriteVarString(" world")

	// Group 2: client=200
	enc.WriteVarUint(1)   // 1 struct
	enc.WriteVarUint(200) // clientID
	enc.WriteVarUint(0)   // startClock

	enc.WriteUint8(wireString) // no flags → explicit parent
	enc.WriteUint8(1)          // named root
	enc.WriteVarString("t")
	enc.WriteVarString("hello")

	enc.WriteVarUint(0) // empty delete set

	doc := New(WithClientID(999))
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))
	assert.Equal(t, "hello world", doc.GetText("t").ToString())
}

func TestUnit_ApplyUpdateV1_GCThenCrossClient(t *testing.T) {
	// Combines both GC structs and cross-client references in one update.
	// Client 100: GC(len=3) at clock 0, then string " end" at clock 3
	//             with origin={200, 1} (cross-client).
	// Client 200: string "ab" at clock 0, parent = root "t".
	enc := encoding.NewEncoder()

	enc.WriteVarUint(2) // 2 client groups

	// Group 1: client=100
	enc.WriteVarUint(2)   // 2 structs
	enc.WriteVarUint(100) // clientID
	enc.WriteVarUint(0)   // startClock

	// GC struct: clock 0, length 3
	enc.WriteUint8(0)
	enc.WriteVarUint(3)

	// String item: clock 3, origin={200, 1}
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(200) // origin client
	enc.WriteVarUint(1)   // origin clock
	enc.WriteVarString(" end")

	// Group 2: client=200
	enc.WriteVarUint(1)   // 1 struct
	enc.WriteVarUint(200) // clientID
	enc.WriteVarUint(0)   // startClock

	enc.WriteUint8(wireString) // no flags → explicit parent
	enc.WriteUint8(1)          // named root
	enc.WriteVarString("t")
	enc.WriteVarString("ab")

	enc.WriteVarUint(0) // empty delete set

	doc := New(WithClientID(999))
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))
	assert.Equal(t, "ab end", doc.GetText("t").ToString())
}

func TestUnit_ApplyUpdateV1_SkipStruct(t *testing.T) {
	// Skip structs (tag 10) represent clock gaps the sender intentionally
	// omits. Verify the V1 decoder handles them without error and that
	// regular items after the skip decode correctly.
	enc := encoding.NewEncoder()

	enc.WriteVarUint(1) // 1 client group
	enc.WriteVarUint(2) // 2 structs
	enc.WriteVarUint(1) // clientID = 1
	enc.WriteVarUint(0) // startClock = 0

	// Skip struct: info byte with tag=10, then VarUint(length)
	enc.WriteUint8(10) // tag=10 (skip), no flags
	enc.WriteVarUint(5) // skip 5 clock values

	// Regular string at clock 5
	enc.WriteUint8(wireString)
	enc.WriteUint8(1) // named root
	enc.WriteVarString("t")
	enc.WriteVarString("hello")

	enc.WriteVarUint(0) // empty delete set

	doc := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))
	assert.Equal(t, "hello", doc.GetText("t").ToString())
}

func TestUnit_ContentDoc_GUID_V1RoundTrip(t *testing.T) {
	// Verify that a subdocument's GUID survives V1 encode → decode.
	// Build a synthetic V1 update with a wireDoc content item.
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // 1 client group
	enc.WriteVarUint(1) // 1 struct
	enc.WriteVarUint(1) // clientID = 1
	enc.WriteVarUint(0) // startClock = 0

	enc.WriteUint8(wireDoc) // tag=9 (wireDoc), no flags
	enc.WriteUint8(1)       // parentInfo=1 → named root
	enc.WriteVarString("subdocs")
	enc.WriteVarBytes([]byte("my-subdoc-id")) // guid

	enc.WriteVarUint(0) // empty delete set

	doc := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))

	// Re-encode and decode again to verify GUID round-trips.
	update2 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := New(WithClientID(3))
	require.NoError(t, ApplyUpdateV1(doc2, update2, nil))

	// Walk the store to find the ContentDoc and verify its GUID.
	items := doc2.store.clients[1]
	require.Len(t, items, 1)
	cd, ok := items[0].Content.(*ContentDoc)
	require.True(t, ok, "expected ContentDoc")
	assert.Equal(t, "my-subdoc-id", cd.Doc.GUID())
}

func TestUnit_WithGUID(t *testing.T) {
	doc := New(WithGUID("test-guid"))
	assert.Equal(t, "test-guid", doc.GUID())

	doc2 := New()
	assert.Equal(t, "", doc2.GUID())
}

func TestUnit_ApplyUpdateV1_CrossClientMultiHop(t *testing.T) {
	// Three clients where dependencies chain: 100→200→300.
	// All items end up in the same YText "t".
	//
	// Client 300: "A" at clock 0 (root, no origin)
	// Client 200: "B" at clock 0, origin={300, 0}
	// Client 100: "C" at clock 0, origin={200, 0}
	//
	// Encoded order: 100, 200, 300. Neither 100 nor 200 can resolve
	// parents on the first pass.
	enc := encoding.NewEncoder()

	enc.WriteVarUint(3) // 3 client groups

	// Group 1: client=100
	enc.WriteVarUint(1)
	enc.WriteVarUint(100)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(200)
	enc.WriteVarUint(0)
	enc.WriteVarString("C")

	// Group 2: client=200
	enc.WriteVarUint(1)
	enc.WriteVarUint(200)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(300)
	enc.WriteVarUint(0)
	enc.WriteVarString("B")

	// Group 3: client=300
	enc.WriteVarUint(1)
	enc.WriteVarUint(300)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireString)
	enc.WriteUint8(1)
	enc.WriteVarString("t")
	enc.WriteVarString("A")

	enc.WriteVarUint(0) // empty delete set

	doc := New(WithClientID(999))
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))
	assert.Equal(t, "ABC", doc.GetText("t").ToString())
}
