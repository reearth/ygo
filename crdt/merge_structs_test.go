package crdt

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// splitRun types s as one item of client 1, then splits it into one item per
// character without letting the transaction merge the halves back, so the
// store holds a run of neighbours that all merge.
func splitRun(t *testing.T, s string) (*Doc, *YText) {
	t.Helper()
	doc := New(WithClientID(1), WithGC(false))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, s, nil) })
	doc.Transact(func(txn *Transaction) {
		for clock := range uint64(len(s) - 1) {
			doc.store.getItemCleanEnd(txn, 1, clock)
		}
		txn.mergeStructs = nil
	})
	require.Len(t, doc.store.clients[1], len(s))
	return doc, txt
}

// Yjs tryToMergeWithLefts carries a merge on to the left while the pairs
// there merge, also past the deleted range it started from.
func TestUnit_MergeClientStructs_CarriesMergeLeft(t *testing.T) {
	doc, txt := splitRun(t, "abcd")
	sv := doc.store.StateVector()
	txn := &Transaction{doc: doc, deleteSet: newDeleteSet(), beforeState: sv, afterState: sv}
	txn.deleteSet.add(ID{Client: 1, Clock: 3}, 1)
	mergeClientStructs(txn, 1)
	structs := doc.store.clients[1]
	require.Len(t, structs, 1)
	assert.Equal(t, "abcd", txt.ToString())
	for _, s := range structs[len(structs):cap(structs)] {
		assert.Nil(t, s, "merged-away structs must not stay reachable")
	}
}

// A split half is merged with the struct after it too (Yjs _mergeStructs).
func TestUnit_MergeTxnStructs_SplitHalfMergesRight(t *testing.T) {
	doc, txt := splitRun(t, "abcd")
	doc.Transact(func(txn *Transaction) {
		txn.mergeStructs = append(txn.mergeStructs, doc.store.clients[1][2])
	})
	require.Len(t, doc.store.clients[1], 1)
	assert.Equal(t, "abcd", txt.ToString())
}

// A restored deletion keeps its own struct: merging the tombstone after it
// would give that tombstone the restored one's redone link (Yjs
// Item.mergeWith refuses a redone left).
func TestUnit_MergeTxnStructs_KeepsRedoneItemApart(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ab", nil) })
	um := NewUndoManager(doc, []SharedType{txt}, WithCaptureTimeout(0))
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 1, 1) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 1) })
	require.True(t, um.Undo())
	require.Equal(t, "a", txt.ToString())
	a, b := doc.store.Find(ID{Client: 1, Clock: 0}), doc.store.Find(ID{Client: 1, Clock: 1})
	require.NotNil(t, a.redone, "a's tombstone links to its restored copy")
	assert.NotSame(t, a, b, "b's tombstone must not merge into the restored a")
	require.True(t, um.Undo())
	assert.Equal(t, "ab", txt.ToString())
}

// A map entry's restored copy chains after the key's entry, so the original
// with the redone link stays right of its deleted predecessor; merging the two
// would lose the link, and the second undo would keep the wrong value. Yjs
// 13.6.32 ends with k=1.
func TestUnit_MergeTxnStructs_KeepsRedoneMapEntryApart(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	m := doc.GetMap("m")
	um := NewUndoManager(doc, []SharedType{m}, WithCaptureTimeout(0))
	for _, step := range []func(*Transaction){
		func(txn *Transaction) { m.Set(txn, "k", 1) },
		func(txn *Transaction) { m.Set(txn, "k", 2) },
		func(txn *Transaction) { m.Delete(txn, "k") },
	} {
		doc.Transact(step)
		um.StopCapturing()
	}
	require.True(t, um.Undo())
	require.True(t, um.Undo())
	v, ok := m.Get("k")
	require.True(t, ok)
	assert.EqualValues(t, 1, v)
}

// Undo finds a restored neighbour by the unit its redone link names, also
// when the copy has since merged into a run. Yjs 13.6.32 ends with [[1,3,2]].
func TestUnit_UndoManager_RedoneNeighbourInsideMergedRun(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	a := doc.GetArray("a")
	um := NewUndoManager(doc, []SharedType{a}, WithCaptureTimeout(0))
	step := func(fn func(*Transaction)) {
		doc.Transact(fn)
		um.StopCapturing()
	}
	step(func(txn *Transaction) { a.InsertType(txn, 0, NewArrayPrelim()) })
	n := a.Get(0).(*YArray)
	step(func(txn *Transaction) { n.Insert(txn, 0, []any{1}) })
	step(func(txn *Transaction) { n.Insert(txn, 1, []any{2}) })
	step(func(txn *Transaction) { n.Insert(txn, 1, []any{3}) })
	step(func(txn *Transaction) { n.Delete(txn, 1, 1) })
	step(func(txn *Transaction) { a.Delete(txn, 0, 1) })
	require.True(t, um.Undo())
	require.True(t, um.Undo())
	got, err := a.ToJSON()
	require.NoError(t, err)
	assert.JSONEq(t, `[[1,3,2]]`, string(got))
}

// A search marker on a merged-away struct moves to the struct it merged into,
// with its index moved back by that struct's length, as in Yjs.
func TestUnit_MergeTxnStructs_MovesMarker(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 3, "d", nil)
		d := doc.store.clients[1][1]
		require.Equal(t, "d", d.Content.(*ContentString).Str)
		txt.markers = []searchMarker{{item: d, index: 3}}
	})
	require.Len(t, doc.store.clients[1], 1)
	require.Len(t, txt.markers, 1)
	assert.Same(t, doc.store.clients[1][0], txt.markers[0].item)
	assert.Equal(t, 0, txt.markers[0].index)
}

// A merged string that is split and merged again keeps only its own text.
func TestUnit_ContentString_AppendAfterSplit(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	for i, s := range []string{"a", "b", "c"} {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, i, s, nil) })
	}
	doc.Transact(func(txn *Transaction) { doc.store.getItemCleanEnd(txn, 1, 0) })
	require.Len(t, doc.store.clients[1], 1)
	assert.Equal(t, "abc", txt.ToString())
}

// Legacy ContentJSON items merge like any other list content.
func TestUnit_MergeTxnStructs_MergesContentJSON(t *testing.T) {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(2) // structs
	enc.WriteVarUint(7) // client
	enc.WriteVarUint(0) // clock
	enc.WriteUint8(wireJSON)
	enc.WriteUint8(1) // root type by name
	enc.WriteVarString("a")
	encodeContent(enc, NewContentJSON("x"), 0)
	enc.WriteUint8(wireJSON | flagHasOrigin)
	enc.WriteVarUint(7)
	enc.WriteVarUint(0)
	encodeContent(enc, NewContentJSON("y"), 0)
	enc.WriteVarUint(0) // delete set
	doc := New()
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))
	require.Equal(t, []any{"x", "y"}, doc.GetArray("a").ToSlice())
	assert.Len(t, doc.store.clients[7], 1)
}

// A struct that starts at the snapshot's clock is not part of the snapshot.
func TestUnit_EncodeStateFromSnapshot_StopsAtSnapshotClock(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ab", nil) })
	snap := CaptureSnapshot(doc)
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "X", nil) })
	update, err := EncodeStateFromSnapshot(doc, snap)
	require.NoError(t, err)
	structs, _, err := decodeStructsV1(New(), update)
	require.NoError(t, err)
	require.Len(t, structs[1], 1)
	assert.Equal(t, 2, structs[1][0].Content.Len())
}

// Undo traces a restored left neighbour through its redone link to the unit
// the link names, over more than one hop. Yjs 13.6.32 gives [0,[60,30]].
func TestUnit_UndoManager_LeftTraceInsideCopiedRun(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	a := doc.GetArray("a")
	um := NewUndoManager(doc, []SharedType{a}, WithCaptureTimeout(0))
	step := func(fn func(*Transaction)) {
		doc.Transact(fn)
		um.StopCapturing()
	}
	step(func(txn *Transaction) { a.InsertType(txn, 0, NewArrayPrelim()) })
	step(func(txn *Transaction) { a.Insert(txn, 0, []any{0}) })
	n := a.Get(1).(*YArray)
	step(func(txn *Transaction) { n.Insert(txn, 0, []any{97, 30}) })
	step(func(txn *Transaction) {
		a.InsertType(txn, 2, NewArrayPrelim())
		a.Delete(txn, 1, 2)
	})
	require.True(t, um.Undo())
	n = a.Get(1).(*YArray)
	step(func(txn *Transaction) { n.Insert(txn, 0, []any{60}) })
	step(func(txn *Transaction) { n.Delete(txn, 1, 1) })
	require.True(t, um.Undo())
	step(func(txn *Transaction) { a.Delete(txn, 0, 2) })
	require.True(t, um.Undo())
	got, err := a.ToJSON()
	require.NoError(t, err)
	assert.JSONEq(t, `[0,[60,30]]`, string(got))
}

// Undo restores the map value Yjs restores once two deleted values of k0
// (clocks 3 and 4) merge into one struct, as in Yjs. The steps and the map
// after each one are those of Yjs 13.6.32; the last undo brings back
// {"k0":61}.
func TestUnit_UndoManager_MapUndoAfterMergedDeletes(t *testing.T) {
	doc := New(WithClientID(1))
	m := doc.GetMap("m")
	um := NewUndoManager(doc, []SharedType{m}, WithCaptureTimeout(time.Hour))
	set := func(key string, v int) func() {
		return func() { doc.Transact(func(txn *Transaction) { m.Set(txn, key, v) }) }
	}
	multi := func(set, del string, v int) func() {
		return func() {
			doc.Transact(func(txn *Transaction) { m.Set(txn, set, v); m.Delete(txn, del) })
		}
	}
	undo := func() { require.True(t, um.Undo()) }
	steps := []struct {
		do   func()
		want string
	}{
		{set("k0", 61), `{"k0":61}`},
		{um.StopCapturing, `{"k0":61}`},
		{multi("k1", "k0", 94), `{"k1":94}`},
		{um.StopCapturing, `{"k1":94}`},
		{multi("k0", "k3", 97), `{"k0":97,"k1":94}`},
		{undo, `{"k1":94}`},
		{func() { require.True(t, um.Redo()) }, `{"k0":97,"k1":94}`},
		{undo, `{"k1":94}`},
		{set("k0", 102), `{"k0":102,"k1":94}`},
		{func() { doc.Transact(func(txn *Transaction) { m.Delete(txn, "k0") }) }, `{"k1":94}`},
		{multi("k2", "k1", 105), `{"k2":105}`},
		{undo, `{"k1":94}`},
		{undo, `{"k0":61}`},
	}
	for i, s := range steps {
		s.do()
		got, err := m.ToJSON()
		require.NoError(t, err)
		require.JSONEq(t, s.want, string(got), "after step %d", i)
	}
}

// A split that a transaction makes and undoes reaches its update as one
// struct, as in Yjs, which merges before it writes the update.
func TestUnit_DocOnUpdate_TransientSplitIsOneStruct(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	var structs []int
	doc.OnUpdate(func(u []byte, _ any) {
		s, _, err := decodeStructsV1(New(), u)
		require.NoError(t, err)
		structs = append(structs, len(s[1]))
	})
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "ab", nil)
		txt.Format(txn, 1, 1, Attributes{"bold": nil})
	})
	assert.Equal(t, []int{1}, structs)
}

// A delete range that starts inside a GC struct leaves it whole, as in Yjs,
// so the state keeps Yjs's bytes. The state holds a Yjs 13.6.32 client 7
// whose nested array's children are GC struct 7:1+3; the delete-only update
// deletes 7:2 again.
func TestUnit_ApplyUpdate_DeleteInsideGCStructKeepsIt(t *testing.T) {
	const state = "01020700010101610100030107010004"
	doc := New(WithClientID(999))
	for _, u := range []string{state, "000107010201"} {
		require.NoError(t, ApplyUpdateV1(doc, mustHex(t, u), nil))
	}
	assertSameBytes(t, "V1 state", EncodeStateAsUpdateV1(doc, nil), state)
}

// Restored neighbours of two clients are two copies, as in Yjs 13.6.32, so a
// concurrent insert between them lands between the copies: [1,9,2].
func TestUnit_UndoManager_RestoresNeighboursOfTwoClientsApart(t *testing.T) {
	a, b, c := New(WithClientID(1)), New(WithClientID(2)), New(WithClientID(3))
	sync := func(from, to *Doc) {
		require.NoError(t, ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, to.StateVector()), nil))
	}
	aa, ba, ca := a.GetArray("a"), b.GetArray("a"), c.GetArray("a")
	um := NewUndoManager(a, []SharedType{aa}, WithCaptureTimeout(0))
	a.Transact(func(txn *Transaction) { aa.Insert(txn, 0, []any{1}) })
	sync(a, b)
	b.Transact(func(txn *Transaction) { ba.Insert(txn, 1, []any{2}) })
	sync(b, a)
	um.StopCapturing()
	a.Transact(func(txn *Transaction) { aa.Delete(txn, 0, 2) })
	sync(a, c)
	c.Transact(func(txn *Transaction) { ca.Insert(txn, 0, []any{9}) })
	require.True(t, um.Undo())
	sync(c, a)
	sync(a, c)
	for _, doc := range []*YArray{aa, ca} {
		got, err := doc.ToJSON()
		require.NoError(t, err)
		assert.JSONEq(t, `[1,9,2]`, string(got))
	}
}

// A remote delete inside a struct that is already deleted leaves it whole, as
// Yjs readAndApplyDeleteSet does. Here the struct has a redone link, so split
// halves would never merge again. Yjs 13.6.32 writes the state below.
func TestUnit_ApplyUpdate_DeleteInsideDeletedRunKeepsIt(t *testing.T) {
	a := New(WithClientID(1))
	ta := a.GetText("t")
	um := NewUndoManager(a, []SharedType{ta}, WithCaptureTimeout(0))
	a.Transact(func(txn *Transaction) { ta.Insert(txn, 0, "abc", nil) })
	um.StopCapturing()
	a.Transact(func(txn *Transaction) { ta.Delete(txn, 0, 3) })
	require.True(t, um.Undo())
	// A peer that saw "abc" deletes "b".
	require.NoError(t, ApplyUpdateV1(a, mustHex(t, "000101010101"), nil))
	assert.Equal(t, "abc", ta.ToString())
	assertSameBytes(t, "V1 state", EncodeStateAsUpdateV1(a, nil), "010201000401017403616263440100036162630101010003")
}

// A move whose target lies inside a GC struct leaves the GC struct whole, as
// Yjs never splits one. The update is client 5's move of 7:2 in root "r".
func TestUnit_ApplyUpdate_MoveTargetInsideGCStructKeepsIt(t *testing.T) {
	doc := New(WithClientID(999))
	for _, u := range []string{"01020700010101610100030107010004", "010105000b01017207020100"} {
		require.NoError(t, ApplyUpdateV1(doc, mustHex(t, u), nil))
	}
	seven := doc.store.clients[7]
	require.Len(t, seven, 2)
	assert.True(t, seven[1].isGCOrphan())
	assert.Equal(t, 3, seven[1].Content.Len())
}

// A merged GC struct takes over the links of the GC struct it absorbs, so no
// item keeps a pointer to a struct that left the store. Crafted input: item
// 9:0 follows GC 7:3+1, which a later update brings; GC 7:1+2, which
// arrived before it, absorbs it.
func TestUnit_MergeTxnStructs_GCAbsorbKeepsLinks(t *testing.T) {
	doc := New(WithClientID(999))
	for _, u := range []string{"0101080008010172017d0100", "01010900c807030800017d0500", "01020700010101610100020107010003", "0101070300010107010301"} {
		require.NoError(t, ApplyUpdateV1(doc, mustHex(t, u), nil))
	}
	x := doc.store.Find(ID{Client: 9, Clock: 0})
	require.NotNil(t, x)
	require.NotNil(t, x.Left)
	assert.Same(t, doc.store.Find(x.Left.ID), x.Left, "x.Left must be a struct in the store")
}

// deleteRestTime times one transaction that deletes n array values, each
// next to values deleted earlier, so all 2n structs merge into one.
func deleteRestTime(t testing.TB, n int) time.Duration {
	doc := New(WithClientID(1))
	a := doc.GetArray("a")
	vals := make([]any, 2*n)
	for i := range vals {
		vals[i] = i
	}
	doc.Transact(func(txn *Transaction) { a.Insert(txn, 0, vals) })
	doc.Transact(func(txn *Transaction) {
		for i := 2*n - 1; i >= 0; i -= 2 {
			a.Delete(txn, i, 1)
		}
	})
	start := time.Now()
	doc.Transact(func(txn *Transaction) { a.Delete(txn, 0, n) })
	elapsed := time.Since(start)
	require.Len(t, doc.store.clients[1], 1)
	return elapsed
}

// Many deleted ranges in one long mergeable run must not each walk the run.
func TestPerf_MergeManyRangesIsLinear(t *testing.T) {
	// Instrumented builds distort the ratio; the benchmark below tracks this.
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	const small, large = 2000, 16000 // 8×: linear ≈ 8×, quadratic ≈ 64×
	// Interleaved, so CPU load during the run hits both sizes alike.
	var s, l time.Duration
	for range 4 {
		s += deleteRestTime(t, small)
		l += deleteRestTime(t, large)
	}
	t.Logf("%v → %v", s, l)
	require.Less(t, l, 24*s+20*time.Millisecond, "deleting many ranges of one run grows superlinearly")
}

func BenchmarkMergeManyRanges(b *testing.B) {
	for _, n := range []int{2000, 16000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var d time.Duration
			for i := 0; i < b.N; i++ {
				d += deleteRestTime(b, n)
			}
			b.ReportMetric(float64(d.Nanoseconds())/float64(b.N), "delete-ns/op")
		})
	}
}
