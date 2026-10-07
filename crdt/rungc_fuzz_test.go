package crdt

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
)

type gcFuzzSide struct {
	d   *Doc
	arr *YArray
	m   *YMap
	txt *YText
	um  *UndoManager
}

func newGCFuzzSide(id ClientID) *gcFuzzSide {
	d := New(WithClientID(id), WithGC(true))
	s := &gcFuzzSide{d: d, arr: d.GetArray("a"), m: d.GetMap("m"), txt: d.GetText("t")}
	s.um = NewUndoManager(d, []SharedType{s.arr, s.m, s.txt}, WithCaptureTimeout(0))
	return s
}

type gcFuzzState struct {
	arr  []any
	get  []any
	m    map[string]any
	txt  string
	alen int
}

func (st gcFuzzState) String() string {
	return fmt.Sprintf("%v|%v|%v|%q|%d", st.arr, st.get, st.m, st.txt, st.alen)
}

func gcFuzzEq(x, y gcFuzzState) bool { return x.String() == y.String() }

func (s *gcFuzzSide) state() gcFuzzState {
	st := gcFuzzState{arr: s.arr.ToSlice(), m: s.m.Entries(), txt: s.txt.ToString(), alen: s.arr.Len()}
	for i := 0; i < s.arr.Len(); i++ {
		st.get = append(st.get, s.arr.Get(i))
	}
	return st
}

var gcFuzzKeys = []string{"k0", "k1", "k2"}

func (s *gcFuzzSide) op(r *rand.Rand, n int) {
	switch r.Intn(9) {
	case 0, 1:
		l := s.arr.Len()
		i := r.Intn(l + 1)
		cnt := 1 + r.Intn(3)
		vals := make([]any, cnt)
		for j := range vals {
			vals[j] = float64(n*10 + j)
		}
		s.d.Transact(func(txn *Transaction) { s.arr.Insert(txn, i, vals) })
	case 2:
		l := s.arr.Len()
		if l == 0 {
			return
		}
		i := r.Intn(l)
		c := 1 + r.Intn(min(3, l-i))
		s.d.Transact(func(txn *Transaction) { s.arr.Delete(txn, i, c) })
	case 3:
		l := s.arr.Len()
		if l < 2 {
			return
		}
		f, to := r.Intn(l), r.Intn(l+1)
		s.d.Transact(func(txn *Transaction) { s.arr.Move(txn, f, to) })
	case 4:
		k := gcFuzzKeys[r.Intn(len(gcFuzzKeys))]
		s.d.Transact(func(txn *Transaction) { s.m.Set(txn, k, float64(n)) })
	case 5:
		k := gcFuzzKeys[r.Intn(len(gcFuzzKeys))]
		s.d.Transact(func(txn *Transaction) { s.m.Delete(txn, k) })
	case 6:
		l := s.txt.Len()
		i := r.Intn(l + 1)
		s.d.Transact(func(txn *Transaction) { s.txt.Insert(txn, i, strconv.Itoa(n%100), nil) })
	case 7:
		l := s.txt.Len()
		if l == 0 {
			return
		}
		i := r.Intn(l)
		s.d.Transact(func(txn *Transaction) { s.txt.Delete(txn, i, 1+r.Intn(min(2, l-i))) })
	case 8:
		if r.Intn(2) == 0 {
			s.um.Undo()
		} else {
			s.um.Redo()
		}
	}
}

// TestFuzzRunGCMerge checks that RunGC's merge pass is unobservable: a doc
// that runs it must match a twin that only runs pass 1, a reload of itself,
// and a synced peer, across array/move/map/text edits and undo/redo.
// FUZZ_ITER overrides the seed count.
func TestFuzzRunGCMerge(t *testing.T) {
	seeds := raceSeeds(300)
	if v := os.Getenv("FUZZ_ITER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			seeds = n
		}
	}
	for seed := 0; seed < seeds; seed++ {
		if err := runGCMergeSeed(int64(seed)); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
	}
}

// gcPass1Only is RunGC without the merge pass.
func gcPass1Only(doc *Doc) {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	for _, items := range doc.store.clients {
		for _, item := range items {
			if item.Deleted {
				if _, ok := item.Content.(*ContentDeleted); !ok {
					item.Content = NewContentDeleted(item.Content.Len())
				}
			}
		}
	}
}

func runGCMergeSeed(seed int64) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	r := rand.New(rand.NewSource(seed))
	a := newGCFuzzSide(20) // runs RunGC
	c := newGCFuzzSide(20) // twin: RunGC pass 1 only, no merge
	b := newGCFuzzSide(10)
	steps := 20 + r.Intn(60)
	for n := 0; n < steps; n++ {
		switch k := r.Intn(10); {
		case k < 5:
			s := r.Int63()
			a.op(rand.New(rand.NewSource(s)), n)
			c.op(rand.New(rand.NewSource(s)), n)
		case k < 7:
			b.op(r, n)
		case k == 7:
			u := EncodeStateAsUpdateV1(b.d, nil)
			if e := ApplyUpdateV1(a.d, u, nil); e != nil {
				return e
			}
			if e := ApplyUpdateV1(c.d, u, nil); e != nil {
				return e
			}
		case k == 8:
			if e := ApplyUpdateV1(b.d, EncodeStateAsUpdateV1(a.d, nil), nil); e != nil {
				return e
			}
		default:
			RunGC(a.d)
			gcPass1Only(c.d)
		}
		sa, sc := a.state(), c.state()
		if !gcFuzzEq(sa, sc) {
			return fmt.Errorf("step %d: RunGC'd doc diverged from twin\n a=%+v\n c=%+v", n, sa, sc)
		}
		if fmt.Sprint(sa.arr) != fmt.Sprint(sa.get) {
			return fmt.Errorf("step %d: Get(i) != ToSlice: %v vs %v", n, sa.get, sa.arr)
		}
		fresh := newGCFuzzSide(30)
		if e := ApplyUpdateV2(fresh.d, EncodeStateAsUpdateV2(a.d, nil), nil); e != nil {
			return e
		}
		if sf := fresh.state(); !gcFuzzEq(sa, sf) {
			return fmt.Errorf("step %d: reload diverged\n a=%+v\n f=%+v", n, sa, sf)
		}
	}
	ua, ub := EncodeStateAsUpdateV1(a.d, nil), EncodeStateAsUpdateV1(b.d, nil)
	_ = ApplyUpdateV1(a.d, ub, nil)
	_ = ApplyUpdateV1(c.d, ub, nil)
	_ = ApplyUpdateV1(b.d, ua, nil)
	sa, sb, sc := a.state(), b.state(), c.state()
	if !gcFuzzEq(sa, sb) || !gcFuzzEq(sa, sc) {
		return fmt.Errorf("final divergence\n a=%+v\n b=%+v\n c=%+v", sa, sb, sc)
	}
	return nil
}
