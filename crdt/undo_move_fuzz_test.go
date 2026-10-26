package crdt

import (
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"strconv"
	"testing"
)

// moveUndoSeeds is the number of seeds each TestFuzz_UndoManager_Moves
// variant runs; override with MOVE_UNDO_ITER for a soak, or set
// MOVE_UNDO_SEED to replay one seed with a step trace.
func moveUndoSeeds() int {
	if v := os.Getenv("MOVE_UNDO_ITER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return raceSeeds(100)
}

// Peers with their own UndoManagers insert, delete, move, re-move, undo and
// redo under shuffled partial delivery, optionally collecting garbage. Each
// step checks the delta mirror; at the end every peer and a fresh V1/V2 load
// of each must agree. The solo variant runs one peer without GC and also
// checks that each undo and redo returns the array to the state it recorded.
func TestFuzz_UndoManager_Moves(t *testing.T) {
	if v := os.Getenv("MOVE_UNDO_SEED"); v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range moveUndoModes {
			if err := moveUndoSeed(seed, m, t.Logf); err != nil {
				t.Errorf("%s: %v", m, err)
			}
		}
		return
	}
	for _, m := range moveUndoModes {
		t.Run(m.String(), func(t *testing.T) {
			fails := 0
			for seed := range uint64(moveUndoSeeds()) {
				if err := moveUndoSeed(seed, m, nil); err != nil {
					if fails++; fails <= 3 {
						t.Errorf("seed %d: %v", seed, err)
					}
				}
			}
			if fails > 0 {
				t.Errorf("%d failing seeds", fails)
			}
		})
	}
}

type moveUndoMode int

const (
	moveUndoPeers moveUndoMode = iota
	moveUndoPeersGC
	moveUndoSolo
)

var moveUndoModes = []moveUndoMode{moveUndoPeers, moveUndoPeersGC, moveUndoSolo}

func (m moveUndoMode) String() string {
	return [...]string{"peers", "peers+gc", "solo"}[m]
}

type moveUndoPeer struct {
	doc       *Doc
	arr       *YArray
	um        *UndoManager
	mir       *deltaMirror
	inbox     [][]byte
	lastMoved any
}

func moveUndoSeed(seed uint64, mode moveUndoMode, logf func(string, ...any)) (err error) {
	r := rand.New(rand.NewPCG(seed, 0x277))
	ids := r.Perm(5)
	gc, solo := mode == moveUndoPeersGC, mode == moveUndoSolo
	peers := make([]*moveUndoPeer, 2+r.IntN(2))
	if solo {
		peers = peers[:1]
	}
	for i := range peers {
		d := newTestDoc(uint64(ids[i] + 1))
		a := d.GetArray("a")
		peers[i] = &moveUndoPeer{doc: d, arr: a, mir: newDeltaMirror(a),
			um: NewUndoManager(d, []SharedType{a}, WithTrackedOrigins("local"))}
	}
	for i, p := range peers {
		p.doc.OnUpdate(func(u []byte, origin any) {
			if origin == "remote" {
				return
			}
			for j, q := range peers {
				if j != i {
					q.inbox = append(q.inbox, append([]byte(nil), u...))
				}
			}
		})
	}
	var trace []string
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic %v after %v", v, trace)
		}
	}()
	deliver := func(p *moveUndoPeer, all bool) error {
		if len(p.inbox) == 0 {
			return nil
		}
		r.Shuffle(len(p.inbox), func(a, b int) { p.inbox[a], p.inbox[b] = p.inbox[b], p.inbox[a] })
		k := len(p.inbox)
		if !all {
			k = 1 + r.IntN(k)
		}
		for _, u := range p.inbox[:k] {
			if err := ApplyUpdateV1(p.doc, u, "remote"); err != nil {
				return err
			}
		}
		p.inbox = append([][]byte(nil), p.inbox[k:]...)
		return nil
	}
	check := func(p *moveUndoPeer, pi int) error {
		got := p.arr.ToSlice()
		switch {
		case p.mir.err != nil:
			return fmt.Errorf("peer %d mirror: %v after %v", pi, p.mir.err, trace)
		case !reflect.DeepEqual(normaliseInts(got), normaliseInts(p.mir.vals)):
			return fmt.Errorf("peer %d mirror %v != ToSlice %v after %v", pi, p.mir.vals, got, trace)
		case p.arr.Len() != len(got):
			return fmt.Errorf("peer %d Len %d != %d after %v", pi, p.arr.Len(), len(got), trace)
		}
		return nil
	}

	// Solo: the states each undo and redo must return to. Its edits are single
	// elements, since even Yjs does not return a run's copy, partly deleted
	// again, to its recorded state.
	var undoTo, redoTo [][]any
	width := 3
	if solo {
		width = 1
	}
	next := 0
	for range 20 + r.IntN(40) {
		pi := r.IntN(len(peers))
		p := peers[pi]
		n := p.arr.Len()
		before, depth := p.arr.ToSlice(), p.um.UndoStackSize()
		var want []any
		var step string
		switch k := r.IntN(12); {
		case k < 3 || n == 0:
			i, vals := r.IntN(n+1), make([]any, 1+r.IntN(width))
			for c := range vals {
				vals[c] = next
				next++
			}
			p.doc.Transact(func(txn *Transaction) { p.arr.Insert(txn, i, vals) }, "local")
			step = fmt.Sprintf("%d.ins(%d,%v)", pi, i, vals)
		case k < 5:
			i := r.IntN(n)
			c := 1 + r.IntN(min(width, 2, n-i))
			p.doc.Transact(func(txn *Transaction) { p.arr.Delete(txn, i, c) }, "local")
			step = fmt.Sprintf("%d.del(%d,%d)", pi, i, c)
		case k < 8:
			from, to := r.IntN(n), r.IntN(n+1)
			if p.lastMoved != nil && r.IntN(2) == 0 {
				for i, v := range p.arr.ToSlice() {
					if reflect.DeepEqual(v, p.lastMoved) {
						from = i
					}
				}
			}
			p.lastMoved = p.arr.ToSlice()[from]
			p.doc.Transact(func(txn *Transaction) { p.arr.Move(txn, from, to) }, "local")
			step = fmt.Sprintf("%d.move(%d,%d)", pi, from, to)
		case k < 10:
			if len(undoTo) > 0 {
				want, undoTo = undoTo[len(undoTo)-1], undoTo[:len(undoTo)-1]
				redoTo = append(redoTo, before)
			}
			p.um.Undo()
			step = fmt.Sprintf("%d.undo", pi)
		case k < 11:
			if len(redoTo) > 0 {
				want, redoTo = redoTo[len(redoTo)-1], redoTo[:len(redoTo)-1]
				undoTo = append(undoTo, before)
			}
			p.um.Redo()
			step = fmt.Sprintf("%d.redo", pi)
		case solo:
			continue
		default:
			if err := deliver(p, r.IntN(2) == 0); err != nil {
				return err
			}
			step = fmt.Sprintf("%d.recv", pi)
		}
		if gc && r.IntN(8) == 0 {
			RunGC(p.doc)
			step += "+gc"
		}
		p.um.StopCapturing()
		trace = append(trace, step)
		if logf != nil {
			logf("%s => %v", step, p.arr.ToSlice())
		}
		if err := check(p, pi); err != nil {
			return err
		}
		if !solo {
			continue
		}
		if want != nil {
			if got := p.arr.ToSlice(); !reflect.DeepEqual(normaliseInts(got), normaliseInts(want)) {
				return fmt.Errorf("got %v, want %v after %v", got, want, trace)
			}
		} else if p.um.UndoStackSize() > depth {
			undoTo, redoTo = append(undoTo, before), nil
		}
	}
	for range 3 {
		for pi, p := range peers {
			if err := deliver(p, true); err != nil {
				return err
			}
			if err := check(p, pi); err != nil {
				return err
			}
		}
	}

	want := normaliseInts(peers[0].arr.ToSlice())
	for pi, p := range peers {
		if got := normaliseInts(p.arr.ToSlice()); !reflect.DeepEqual(got, want) {
			return fmt.Errorf("peer %d %v diverges from peer 0 %v after %v", pi, got, want, trace)
		}
		v1, v2 := New(), New()
		if err := ApplyUpdateV1(v1, EncodeStateAsUpdateV1(p.doc, nil), nil); err != nil {
			return err
		}
		if err := ApplyUpdateV2(v2, EncodeStateAsUpdateV2(p.doc, nil), nil); err != nil {
			return err
		}
		for name, d := range map[string]*Doc{"V1": v1, "V2": v2} {
			if got := normaliseInts(d.GetArray("a").ToSlice()); !reflect.DeepEqual(got, want) {
				return fmt.Errorf("fresh %s load of peer %d %v != %v after %v", name, pi, got, want, trace)
			}
		}
	}
	return nil
}
