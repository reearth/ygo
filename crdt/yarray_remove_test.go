package crdt

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// A client's later move of an element supersedes its earlier one.
func TestUnit_YArray_ReMove_SameClientTakesEffect(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	m := newDeltaMirror(arr)
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c", "d"}) })

	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 3) })
	requireArrayConsistent(t, arr, []any{"b", "c", "d", "a"})
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 3, 0) })
	requireArrayConsistent(t, arr, []any{"a", "b", "c", "d"})
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 2) })
	requireArrayConsistent(t, arr, []any{"b", "c", "a", "d"})
	require.NoError(t, m.err)
	require.Equal(t, arr.ToSlice(), m.vals)

	fresh := newTestDoc(2)
	syncTo(t, doc, fresh)
	requireArrayConsistent(t, fresh.GetArray("a"), []any{"b", "c", "a", "d"})
}

// Undoing a re-move returns the element to its previous destination.
func TestUnit_UndoManager_UndoReMove_RestoresPreviousMove(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	m := newDeltaMirror(arr)
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a", "b", "c", "d"}) })
	um := NewUndoManager(doc, []SharedType{arr})

	doc.Transact(func(txn *Transaction) { arr.Move(txn, 0, 3) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { arr.Move(txn, 3, 1) })
	requireArrayConsistent(t, arr, []any{"b", "a", "c", "d"})

	steps := []struct {
		op   func() bool
		want []any
	}{
		{um.Undo, []any{"b", "c", "d", "a"}},
		{um.Undo, []any{"a", "b", "c", "d"}},
		{um.Redo, []any{"b", "c", "d", "a"}},
		{um.Redo, []any{"b", "a", "c", "d"}},
	}
	for i, s := range steps {
		require.True(t, s.op(), "step %d", i)
		requireArrayConsistent(t, arr, s.want)
		require.NoError(t, m.err)
		require.Equal(t, arr.ToSlice(), m.vals, "step %d delta", i)
	}
	fresh := newTestDoc(2)
	syncTo(t, doc, fresh)
	requireArrayConsistent(t, fresh.GetArray("a"), []any{"b", "a", "c", "d"})
}

// modelMove applies Move's contract to a plain slice: the element lands at to.
func modelMove(s []any, from, to int) []any {
	v := s[from]
	rest := append(append([]any{}, s[:from]...), s[from+1:]...)
	if to > len(rest) {
		to = len(rest)
	}
	return spliceInto(rest, to, []any{v})
}

// TestMoveModelSweep checks a single peer's inserts, deletes and (re-)moves
// against a plain-slice model, so a move with no effect fails.
func TestMoveModelSweep(t *testing.T) {
	for seed := 0; seed < moveDeltaSeeds(); seed++ {
		r := rand.New(rand.NewPCG(uint64(seed), 0x276))
		doc := newTestDoc(1)
		arr := doc.GetArray("a")
		mirror := newDeltaMirror(arr)
		var model []any
		var log []string
		next := int64(0)
		for step := 0; step < 40; step++ {
			n := len(model)
			switch k := r.IntN(5); {
			case k == 0 || n < 2:
				i := r.IntN(n + 1)
				doc.Transact(func(txn *Transaction) { arr.Insert(txn, i, []any{next}) })
				model = spliceInto(model, i, []any{next})
				next++
				log = append(log, fmt.Sprintf("ins(%d)", i))
			case k == 1:
				i := r.IntN(n)
				doc.Transact(func(txn *Transaction) { arr.Delete(txn, i, 1) })
				model = append(model[:i:i], model[i+1:]...)
				log = append(log, fmt.Sprintf("del(%d)", i))
			default:
				from, to := r.IntN(n), r.IntN(n+1)
				doc.Transact(func(txn *Transaction) { arr.Move(txn, from, to) })
				if from != to {
					model = modelMove(model, from, to)
				}
				log = append(log, fmt.Sprintf("move(%d,%d)", from, to))
			}
			got := arr.ToSlice()
			if len(got) != len(model) || (len(got) > 0 && !reflect.DeepEqual(got, model)) {
				t.Fatalf("seed %d: ToSlice %v != model %v after %v", seed, got, model, log)
			}
			if mirror.err != nil || !reflect.DeepEqual(normaliseInts(got), normaliseInts(mirror.vals)) {
				t.Fatalf("seed %d: mirror %v (%v) != %v after %v", seed, mirror.vals, mirror.err, got, log)
			}
			if arr.Len() != len(model) {
				t.Fatalf("seed %d: Len %d != %d after %v", seed, arr.Len(), len(model), log)
			}
			for i := range model {
				if g := arr.Get(i); g != model[i] {
					t.Fatalf("seed %d: Get(%d)=%v want %v after %v", seed, i, g, model[i], log)
				}
			}
		}
	}
}

// TestInteg_YArray_ReMove_ShuffledArrival records every transaction's update
// from peers that re-move, undo and edit concurrently, then replays them into
// fresh docs in shuffled orders: each must match the fully synced peers.
func TestInteg_YArray_ReMove_ShuffledArrival(t *testing.T) {
	for seed := 0; seed < moveDeltaSeeds()/3; seed++ {
		r := rand.New(rand.NewPCG(uint64(seed), 0x2761))
		var peers []*Doc
		var arrs []*YArray
		var ums []*UndoManager
		var updates [][]byte
		for c := 1; c <= 3; c++ {
			d := newTestDoc(uint64(c))
			d.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
			peers = append(peers, d)
			arrs = append(arrs, d.GetArray("a"))
			ums = append(ums, NewUndoManager(d, []SharedType{d.GetArray("a")}, WithTrackedOrigins("local")))
		}
		peers[0].Transact(func(txn *Transaction) { arrs[0].Push(txn, []any{"a", "b", "c", "d", "e"}) })
		for _, p := range peers[1:] {
			syncTo(t, peers[0], p)
		}
		lastTo := make([]int, 3)
		for step := 0; step < 30; step++ {
			p := r.IntN(3)
			d, arr := peers[p], arrs[p]
			n := arr.Len()
			switch k := r.IntN(8); {
			case k < 3 && n >= 2:
				// Bias toward re-moving the element this peer moved last.
				from := min(lastTo[p], n-1)
				if r.IntN(2) == 0 {
					from = r.IntN(n)
				}
				to := r.IntN(n)
				lastTo[p] = to
				d.Transact(func(txn *Transaction) { arr.Move(txn, from, to) }, "local")
				ums[p].StopCapturing()
			case k == 3:
				ums[p].Undo()
			case k == 4:
				ums[p].Redo()
			case k == 5 && n > 0:
				i := r.IntN(n)
				d.Transact(func(txn *Transaction) { arr.Delete(txn, i, 1) }, "local")
				ums[p].StopCapturing()
			case k == 6:
				i := r.IntN(n + 1)
				d.Transact(func(txn *Transaction) { arr.Insert(txn, i, []any{fmt.Sprint(seed, step)}) }, "local")
				ums[p].StopCapturing()
			default:
				q := (p + 1 + r.IntN(2)) % 3
				syncTo(t, peers[p], peers[q])
			}
		}
		for range 2 {
			for i := range peers {
				for j := range peers {
					if i != j {
						syncTo(t, peers[i], peers[j])
					}
				}
			}
		}
		want := arrs[0].ToSlice()
		for i := 1; i < 3; i++ {
			require.Equal(t, want, arrs[i].ToSlice(), "seed %d: peer %d diverged", seed, i)
		}
		for perm := 0; perm < 4; perm++ {
			order := append([][]byte{}, updates...)
			r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			fresh := newTestDoc(9)
			for _, u := range order {
				require.NoError(t, ApplyUpdateV1(fresh, u, nil))
			}
			requireArrayConsistent(t, fresh.GetArray("a"), want)
		}
	}
}
