package crdt

import (
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"strconv"
	"testing"
)

// undoHarnessSeeds is the number of seeds TestUndoGetConsistencySweep runs;
// override with UNDO_HARNESS_ITER for a soak.
func undoHarnessSeeds() int {
	if v := os.Getenv("UNDO_HARNESS_ITER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 500
}

// getAsJSON renders Get(i) in ToSlice's representation.
func getAsJSON(v any) any {
	switch x := v.(type) {
	case *YArray:
		return toJSONValue(&ContentType{Type: &x.abstractType})
	case *YMap:
		return toJSONValue(&ContentType{Type: &x.abstractType})
	case *YText:
		return toJSONValue(&ContentType{Type: &x.abstractType})
	}
	return v
}

type undoOpKind int

const (
	uoInsert undoOpKind = iota
	uoInsertType
	uoDelete
	uoMove
	uoUndo
	uoRedo
	uoStop
)

type undoOp struct {
	kind undoOpKind
	a, b int // index/len, from/to
	v    int // first value
}

func (o undoOp) String() string {
	switch o.kind {
	case uoInsert:
		return fmt.Sprintf("ins(%d,%d×%d)", o.a, o.v, o.b)
	case uoInsertType:
		return fmt.Sprintf("insType(%d,%d)", o.a, o.v)
	case uoDelete:
		return fmt.Sprintf("del(%d,%d)", o.a, o.b)
	case uoMove:
		return fmt.Sprintf("move(%d,%d)", o.a, o.b)
	case uoUndo:
		return "undo"
	case uoRedo:
		return "redo"
	}
	return "stop"
}

// randUndoOp draws one op valid for an array of length n.
func randUndoOp(r *rand.Rand, n int, next *int) undoOp {
	for {
		switch k := r.IntN(10); {
		case k < 3:
			o := undoOp{kind: uoInsert, a: r.IntN(n + 1), b: 1 + r.IntN(3), v: *next}
			*next += o.b
			return o
		case k == 3:
			o := undoOp{kind: uoInsertType, a: r.IntN(n + 1), v: *next}
			*next += 2
			return o
		case k < 6 && n > 0:
			a := r.IntN(n)
			return undoOp{kind: uoDelete, a: a, b: 1 + r.IntN(min(3, n-a))}
		case k == 6 && n > 1:
			return undoOp{kind: uoMove, a: r.IntN(n), b: r.IntN(n + 1)}
		case k == 7:
			return undoOp{kind: uoUndo}
		case k == 8:
			return undoOp{kind: uoRedo}
		case k == 9:
			return undoOp{kind: uoStop}
		}
	}
}

func applyUndoOp(doc *Doc, arr *YArray, um *UndoManager, o undoOp) {
	n := arr.Len()
	switch o.kind {
	case uoInsert:
		vals := make([]any, o.b)
		for i := range vals {
			vals[i] = o.v + i
		}
		doc.Transact(func(txn *Transaction) { arr.Insert(txn, min(o.a, n), vals) })
	case uoInsertType:
		doc.Transact(func(txn *Transaction) {
			m := NewMapPrelim()
			m.Set(txn, "k", o.v)
			sub := NewArrayPrelim()
			sub.Push(txn, []any{o.v + 1})
			m.Set(txn, "s", sub)
			arr.InsertType(txn, min(o.a, n), m)
		})
	case uoDelete:
		if o.a < n {
			doc.Transact(func(txn *Transaction) { arr.Delete(txn, o.a, min(o.b, n-o.a)) })
		}
	case uoMove:
		if o.a < n {
			doc.Transact(func(txn *Transaction) { arr.Move(txn, o.a, min(o.b, n)) })
		}
	case uoUndo:
		um.Undo()
	case uoRedo:
		um.Redo()
	case uoStop:
		um.StopCapturing()
	}
}

// checkArrayGet reports a Len/Get(i) disagreement with ToSlice, or a panic.
func checkArrayGet(arr *YArray) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	want := arr.ToSlice()
	if got := arr.Len(); got != len(want) {
		return fmt.Errorf("Len()=%d, ToSlice()=%v", got, want)
	}
	for i := range want {
		if got := getAsJSON(arr.Get(i)); !reflect.DeepEqual(got, want[i]) {
			return fmt.Errorf("Get(%d)=%v, ToSlice()=%v", i, got, want)
		}
	}
	return nil
}

// replayUndoOps applies ops, checking consistency after every step.
func replayUndoOps(ops []undoOp) (err error) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	um := NewUndoManager(doc, []SharedType{arr})
	defer um.Destroy()
	for i, o := range ops {
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("step %d (%v): panic: %v", i, o, p)
				}
			}()
			applyUndoOp(doc, arr, um, o)
		}()
		if err != nil {
			return err
		}
		if e := checkArrayGet(arr); e != nil {
			return fmt.Errorf("step %d (%v): %w", i, o, e)
		}
	}
	return nil
}

// genUndoOps draws a 60-op scenario for seed.
func genUndoOps(seed uint64) []undoOp {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	um := NewUndoManager(doc, []SharedType{arr})
	defer um.Destroy()
	ops := make([]undoOp, 0, 60)
	next := 0
	for len(ops) < 60 {
		o := randUndoOp(r, arr.Len(), &next)
		ops = append(ops, o)
		applyUndoOp(doc, arr, um, o)
	}
	return ops
}

// minimiseUndoOps greedily drops ops while the scenario still fails.
func minimiseUndoOps(ops []undoOp) []undoOp {
	for changed := true; changed; {
		changed = false
		for i := len(ops) - 1; i >= 0; i-- {
			cand := append(append([]undoOp(nil), ops[:i]...), ops[i+1:]...)
			if replayUndoOps(cand) != nil {
				ops, changed = cand, true
			}
		}
	}
	return ops
}

// TestUndoGetConsistencySweep: random inserts, nested inserts, deletes, moves
// and undo/redo on a root array must keep Len/Get(i) equal to ToSlice().
func TestUndoGetConsistencySweep(t *testing.T) {
	for seed := 0; seed < undoHarnessSeeds(); seed++ {
		ops := genUndoOps(uint64(seed))
		if err := replayUndoOps(ops); err != nil {
			min := minimiseUndoOps(ops)
			t.Fatalf("seed %d: %v\nminimised ops: %v\nminimised failure: %v", seed, err, min, replayUndoOps(min))
		}
	}
}
