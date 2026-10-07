package crdt

import (
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// moveDeltaSeeds is the number of seeds TestMoveDeltaMirrorSweep runs;
// override with MOVE_DELTA_ITER for a soak.
func moveDeltaSeeds() int {
	if v := os.Getenv("MOVE_DELTA_ITER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return raceSeeds(300)
}

// applyArrayDelta replays one YArrayEvent delta onto a plain slice.
func applyArrayDelta(mirror []any, delta []Delta) ([]any, error) {
	out := make([]any, 0, len(mirror))
	i := 0
	for _, d := range delta {
		switch d.Op {
		case DeltaOpRetain:
			if i+d.Retain > len(mirror) {
				return nil, fmt.Errorf("retain %d past end (at %d of %d)", d.Retain, i, len(mirror))
			}
			out = append(out, mirror[i:i+d.Retain]...)
			i += d.Retain
		case DeltaOpDelete:
			if i+d.Delete > len(mirror) {
				return nil, fmt.Errorf("delete %d past end (at %d of %d)", d.Delete, i, len(mirror))
			}
			i += d.Delete
		case DeltaOpInsert:
			out = append(out, d.Insert.([]any)...)
		}
	}
	return append(out, mirror[i:]...), nil
}

// deltaMirror keeps a slice in step with arr using only its emitted deltas.
type deltaMirror struct {
	vals []any
	err  error
}

func newDeltaMirror(arr *YArray) *deltaMirror {
	m := &deltaMirror{}
	arr.Observe(func(e YArrayEvent) {
		if m.err != nil {
			return
		}
		m.vals, m.err = applyArrayDelta(m.vals, e.Delta)
	})
	return m
}

// Undoing a Move must emit the element's return to its origin.
func TestUnit_YArrayEvent_UndoMove_EmitsDelta(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	m := newDeltaMirror(arr)
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{0, 1, 2, 3}) })
	um := NewUndoManager(doc, []SharedType{arr})

	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 3) })
	require.NoError(t, m.err)
	require.Equal(t, arr.ToSlice(), m.vals)

	require.True(t, um.Undo())
	require.Equal(t, []any{int64(0), int64(1), int64(2), int64(3)}, arr.ToSlice())
	require.NoError(t, m.err)
	require.Equal(t, arr.ToSlice(), m.vals)

	require.True(t, um.Redo())
	require.NoError(t, m.err)
	require.Equal(t, arr.ToSlice(), m.vals)
}

// A remote delete of the winning move hands the target to the next
// live move; the delta must show it leaving one destination for the other.
func TestInteg_YArrayEvent_RemoteDeleteOfWinningMove_EmitsDelta(t *testing.T) {
	docA, docB := newTestDoc(1), newTestDoc(2)
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
	docA.Transact(func(txn *Transaction) { arrA.Push(txn, []any{"a", "b", "c"}) })
	syncTo(t, docA, docB)
	um := NewUndoManager(docA, []SharedType{arrA}, WithTrackedOrigins("local"))
	docA.Transact(func(txn *Transaction) { arrA.Move(txn, 0, 3) }, "local")
	docB.Transact(func(txn *Transaction) { arrB.Move(txn, 0, 1) })
	syncTo(t, docA, docB)
	syncTo(t, docB, docA)

	m := newDeltaMirror(arrB)
	m.vals = arrB.ToSlice()
	require.True(t, um.Undo())
	syncTo(t, docA, docB)
	require.Equal(t, []any{"b", "a", "c"}, arrB.ToSlice())
	require.NoError(t, m.err)
	require.Equal(t, arrB.ToSlice(), m.vals)
}

// Deleting a moved element removes it at its rendered destination.
func TestUnit_YArrayEvent_DeleteMovedElement_EmitsDelta(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	m := newDeltaMirror(arr)
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{0, 1, 2, 3}) })
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 3) })
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 3, 1) })
	require.Equal(t, []any{int64(1), int64(2), int64(3)}, arr.ToSlice())
	require.NoError(t, m.err)
	require.Equal(t, arr.ToSlice(), m.vals)
}

type mdOpKind int

const (
	mdInsert mdOpKind = iota
	mdDelete
	mdMove
	mdUndo
	mdRedo
	mdRemote // B edits, then B's updates apply to A
	mdPush   // A's updates apply to B
)

// TestMoveDeltaMirrorSweep drives random local edits, moves, undo/redo and
// remote applies into A and asserts a mirror built only from A's deltas always
// equals A.ToSlice(), then that A, B and fresh V1/V2 reloads converge.
func TestMoveDeltaMirrorSweep(t *testing.T) {
	for seed := 0; seed < moveDeltaSeeds(); seed++ {
		if err := runMoveDeltaSeed(uint64(seed)); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
	}
}

func runMoveDeltaSeed(seed uint64) (err error) {
	r := rand.New(rand.NewPCG(seed, 0x275))
	// Client IDs come from their own stream so each seed's ops stay fixed.
	ids := rand.New(rand.NewPCG(seed, 0x1d))
	idA, idB := ids.Uint64N(1<<32), ids.Uint64N(1<<32)
	for idB == idA {
		idB = ids.Uint64N(1 << 32)
	}
	docA, docB := newTestDoc(idA), newTestDoc(idB)
	arrA, arrB := docA.GetArray("a"), docB.GetArray("a")
	m := newDeltaMirror(arrA)
	um := NewUndoManager(docA, []SharedType{arrA}, WithTrackedOrigins("local"))
	next := 0
	var log []string
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic %v after %v", p, log)
		}
	}()
	edit := func(doc *Doc, arr *YArray, origin any) string {
		n := arr.Len()
		switch k := r.IntN(6); {
		case k < 2 || n == 0:
			i := r.IntN(n + 1)
			doc.Transact(func(txn *Transaction) { arr.Insert(txn, i, []any{next}) }, origin)
			next++
			return fmt.Sprintf("ins(%d)", i)
		case k == 2:
			i := r.IntN(n)
			doc.Transact(func(txn *Transaction) { arr.Delete(txn, i, 1) }, origin)
			return fmt.Sprintf("del(%d)", i)
		default:
			from, to := r.IntN(n), r.IntN(n)
			doc.Transact(func(txn *Transaction) { arr.Move(txn, from, to) }, origin)
			return fmt.Sprintf("move(%d,%d)", from, to)
		}
	}
	for step := 0; step < 40; step++ {
		var s string
		switch k := mdOpKind(r.IntN(7)); k {
		case mdInsert, mdDelete, mdMove:
			s = "A." + edit(docA, arrA, "local")
			um.StopCapturing()
		case mdUndo:
			um.Undo()
			s = "undo"
		case mdRedo:
			um.Redo()
			s = "redo"
		case mdRemote:
			s = "B." + edit(docB, arrB, nil) + ">A"
			if e := ApplyUpdateV1(docA, EncodeStateAsUpdateV1(docB, docA.StateVector()), nil); e != nil {
				return e
			}
		case mdPush:
			s = "A>B"
			if e := ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, docB.StateVector()), nil); e != nil {
				return e
			}
		}
		log = append(log, s)
		if m.err != nil {
			return fmt.Errorf("%v after %v", m.err, log)
		}
		if got := arrA.ToSlice(); !reflect.DeepEqual(normaliseInts(got), normaliseInts(m.vals)) {
			return fmt.Errorf("mirror %v != ToSlice %v after %v", m.vals, got, log)
		}
	}
	if e := ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, docB.StateVector()), nil); e != nil {
		return e
	}
	if e := ApplyUpdateV1(docA, EncodeStateAsUpdateV1(docB, docA.StateVector()), nil); e != nil {
		return e
	}
	want := normaliseInts(arrA.ToSlice())
	if got := normaliseInts(arrB.ToSlice()); !reflect.DeepEqual(got, want) {
		return fmt.Errorf("ids %d/%d: A %v != B %v after %v", idA, idB, want, got, log)
	}
	for _, rt := range []struct {
		name  string
		bytes []byte
		apply func(*Doc, []byte, any) error
	}{{"V1", EncodeStateAsUpdateV1(docA, nil), ApplyUpdateV1}, {"V2", EncodeStateAsUpdateV2(docA, nil), ApplyUpdateV2}} {
		fresh := New()
		if e := rt.apply(fresh, rt.bytes, nil); e != nil {
			return e
		}
		if got := normaliseInts(fresh.GetArray("a").ToSlice()); !reflect.DeepEqual(got, want) {
			return fmt.Errorf("ids %d/%d: fresh %s reload %v != A %v after %v", idA, idB, rt.name, got, want, log)
		}
	}
	return nil
}

// normaliseInts folds the int / int64 / float64 spellings a value takes
// across a wire round-trip into one so equal arrays compare equal.
func normaliseInts(vs []any) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		switch x := v.(type) {
		case int:
			out[i] = float64(x)
		case int64:
			out[i] = float64(x)
		default:
			out[i] = v
		}
	}
	return out
}
