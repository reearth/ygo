package crdt

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
)

// lazyRecord runs fn in a transaction on d and returns the V1 update it emitted.
func lazyRecord(t *testing.T, d *Doc, fn func(txn *Transaction)) []byte {
	t.Helper()
	var u []byte
	un := d.OnUpdate(func(x []byte, _ any) { u = append([]byte(nil), x...) })
	d.Transact(fn)
	un()
	if u == nil {
		t.Fatal("transaction emitted no update")
	}
	return u
}

// lazyApply applies a V1 update to d, converting it to V2 first when v2 is set.
func lazyApply(t *testing.T, d *Doc, u []byte, v2 bool) {
	t.Helper()
	var err error
	if v2 {
		var u2 []byte
		if u2, err = UpdateV1ToV2(u); err == nil {
			err = ApplyUpdateV2(d, u2, "remote")
		}
	} else {
		err = ApplyUpdateV1(d, u, "remote")
	}
	if err != nil {
		t.Fatal(err)
	}
}

// staleRootParents lists every struct, integrated or parked, whose parent is
// a root type other than the one d.share holds for that name.
func staleRootParents(d *Doc) []string {
	var out []string
	check := func(where string, it *Item) {
		p := it.Parent
		if p == nil || p.item != nil {
			return
		}
		if cur, ok := d.share[p.name]; !ok || cur.baseType() != p {
			out = append(out, fmt.Sprintf("%s %v parented to a discarded %q", where, it.ID, p.name))
		}
	}
	for _, items := range d.store.clients {
		for _, it := range items {
			check("integrated", it)
		}
	}
	if d.store.pending != nil {
		for _, it := range d.store.pending.items {
			check("pending", it)
		}
	}
	return out
}

type lazyKind int

const (
	lazyText lazyKind = iota
	lazyArray
	lazyMap
	lazyXML
)

func (k lazyKind) root() string { return [...]string{"t", "a", "m", "x"}[k] }

// lazyGet resolves root kind k on d, through txn when one is given.
func lazyGet(d *Doc, txn *Transaction, k lazyKind) {
	name := k.root()
	switch {
	case txn != nil && k == lazyText:
		txn.GetText(name)
	case txn != nil && k == lazyArray:
		txn.GetArray(name)
	case txn != nil && k == lazyMap:
		txn.GetMap(name)
	case txn != nil:
		txn.GetXmlFragment(name)
	case k == lazyText:
		d.GetText(name)
	case k == lazyArray:
		d.GetArray(name)
	case k == lazyMap:
		d.GetMap(name)
	default:
		d.GetXmlFragment(name)
	}
}

// lazyView renders root kind k of d.
func lazyView(t *testing.T, d *Doc, k lazyKind) string {
	t.Helper()
	var b []byte
	var err error
	switch k {
	case lazyText:
		return fmt.Sprint(d.GetText(k.root()).ToDelta())
	case lazyArray:
		b, err = d.GetArray(k.root()).ToJSON()
	case lazyMap:
		b, err = d.GetMap(k.root()).ToJSON()
	default:
		return d.GetXmlFragment(k.root()).ToXML()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lazyCheck requires a to match want on root k, hold no stale parent, and
// reload identically through both encodings.
func lazyCheck(t *testing.T, a *Doc, k lazyKind, want string) {
	t.Helper()
	if got := lazyView(t, a, k); got != want {
		t.Fatalf("root %q: got %s, want %s", k.root(), got, want)
	}
	if s := staleRootParents(a); len(s) > 0 {
		t.Fatalf("stale parents: %v", s)
	}
	for _, v2 := range []bool{false, true} {
		r := New()
		if v2 {
			if err := ApplyUpdateV2(r, EncodeStateAsUpdateV2(a, nil), nil); err != nil {
				t.Fatal(err)
			}
		} else if err := ApplyUpdateV1(r, EncodeStateAsUpdateV1(a, nil), nil); err != nil {
			t.Fatal(err)
		}
		if got := lazyView(t, r, k); got != want {
			t.Fatalf("reload (v2=%v) of %q: got %s, want %s", v2, k.root(), got, want)
		}
	}
}

// lazyVariants runs fn for each apply encoding and root accessor.
func lazyVariants(t *testing.T, fn func(t *testing.T, v2 bool, get func(d *Doc, k lazyKind))) {
	for _, v2 := range []bool{false, true} {
		for _, viaTxn := range []bool{false, true} {
			get := func(d *Doc, k lazyKind) { lazyGet(d, nil, k) }
			if viaTxn {
				get = func(d *Doc, k lazyKind) { d.Transact(func(txn *Transaction) { lazyGet(d, txn, k) }) }
			}
			t.Run(fmt.Sprintf("v2=%v/txn=%v", v2, viaTxn), func(t *testing.T) { fn(t, v2, get) })
		}
	}
}

// lazyGapped records three edits on a fresh peer and delivers them to a fresh
// receiver as 0, 2 (parked on the missing 1), first access, 1.
func lazyGapped(t *testing.T, k lazyKind, edits [3]func(d *Doc, txn *Transaction)) {
	lazyVariants(t, func(t *testing.T, v2 bool, get func(*Doc, lazyKind)) {
		b := New(WithClientID(2))
		lazyGet(b, nil, k)
		var u [3][]byte
		for i, e := range edits {
			u[i] = lazyRecord(t, b, func(txn *Transaction) { e(b, txn) })
		}
		a := New(WithClientID(1))
		lazyApply(t, a, u[0], v2)
		lazyApply(t, a, u[2], v2)
		if a.store.pending == nil {
			t.Fatal("edit 2 did not park")
		}
		get(a, k)
		lazyApply(t, a, u[1], v2)
		lazyCheck(t, a, k, lazyView(t, b, k))
	})
}

func TestLazyRoot_ParkedText(t *testing.T) {
	lazyGapped(t, lazyText, [3]func(*Doc, *Transaction){
		func(d *Doc, txn *Transaction) { d.share["t"].(*YText).Insert(txn, 0, "a", nil) },
		func(d *Doc, txn *Transaction) { d.share["t"].(*YText).Insert(txn, 1, "b", nil) },
		func(d *Doc, txn *Transaction) { d.share["t"].(*YText).Insert(txn, 0, "c", nil) },
	})
}

func TestLazyRoot_ParkedArray(t *testing.T) {
	lazyGapped(t, lazyArray, [3]func(*Doc, *Transaction){
		func(d *Doc, txn *Transaction) { d.share["a"].(*YArray).Insert(txn, 0, []any{1}) },
		func(d *Doc, txn *Transaction) { d.share["a"].(*YArray).Insert(txn, 1, []any{2}) },
		func(d *Doc, txn *Transaction) { d.share["a"].(*YArray).Insert(txn, 0, []any{3}) },
	})
}

func TestLazyRoot_ParkedNestedType(t *testing.T) {
	lazyGapped(t, lazyArray, [3]func(*Doc, *Transaction){
		func(d *Doc, txn *Transaction) { d.share["a"].(*YArray).Insert(txn, 0, []any{1}) },
		func(d *Doc, txn *Transaction) { d.share["a"].(*YArray).Insert(txn, 1, []any{2}) },
		func(d *Doc, txn *Transaction) {
			m := NewMapPrelim()
			m.Set(nil, "k", "v")
			d.share["a"].(*YArray).InsertType(txn, 0, m)
		},
	})
}

func TestLazyRoot_ParkedXmlFragment(t *testing.T) {
	lazyGapped(t, lazyXML, [3]func(*Doc, *Transaction){
		func(d *Doc, txn *Transaction) {
			d.share["x"].(*YXmlFragment).InsertElement(txn, 0, NewYXmlElement("p"))
		},
		func(d *Doc, txn *Transaction) {
			d.share["x"].(*YXmlFragment).InsertElement(txn, 1, NewYXmlElement("q"))
		},
		func(d *Doc, txn *Transaction) {
			d.share["x"].(*YXmlFragment).InsertElement(txn, 0, NewYXmlElement("r"))
		},
	})
}

func TestLazyRoot_ParkedMove(t *testing.T) {
	lazyGapped(t, lazyArray, [3]func(*Doc, *Transaction){
		func(d *Doc, txn *Transaction) { d.share["a"].(*YArray).Push(txn, []any{"x"}) },
		func(d *Doc, txn *Transaction) { d.share["a"].(*YArray).Push(txn, []any{"y"}) },
		func(d *Doc, txn *Transaction) { d.share["a"].(*YArray).Move(txn, 1, 0) },
	})
}

// A parked overwrite inherits its parent from the key's earlier entry, and
// must still reach the map's observers once it integrates.
func TestLazyRoot_ParkedMapKey(t *testing.T) {
	set := func(k string, v any) func(*Doc, *Transaction) {
		return func(d *Doc, txn *Transaction) { d.share["m"].(*YMap).Set(txn, k, v) }
	}
	lazyGapped(t, lazyMap, [3]func(*Doc, *Transaction){set("k", "a"), set("j", 1), set("k", "b")})

	lazyVariants(t, func(t *testing.T, v2 bool, get func(*Doc, lazyKind)) {
		b := New(WithClientID(2))
		mb := b.GetMap("m")
		u0 := lazyRecord(t, b, func(txn *Transaction) { mb.Set(txn, "k", "a") })
		u1 := lazyRecord(t, b, func(txn *Transaction) { mb.Set(txn, "j", 1) })
		u2 := lazyRecord(t, b, func(txn *Transaction) { mb.Set(txn, "k", "b") })
		a := New(WithClientID(1))
		lazyApply(t, a, u0, v2)
		lazyApply(t, a, u2, v2)
		get(a, lazyMap)
		changed := map[string]bool{}
		a.GetMap("m").Observe(func(e YMapEvent) {
			for key := range e.KeysChanged {
				changed[key] = true
			}
		})
		lazyApply(t, a, u1, v2)
		if !changed["j"] || !changed["k"] {
			t.Fatalf("observer saw %v, want j and k", changed)
		}
		lazyCheck(t, a, lazyMap, lazyView(t, b, lazyMap))
	})
}

func TestLazyRoot_ParkedSubdoc(t *testing.T) {
	lazyVariants(t, func(t *testing.T, v2 bool, get func(*Doc, lazyKind)) {
		b := New(WithClientID(2))
		mb := b.GetMap("m")
		u0 := lazyRecord(t, b, func(txn *Transaction) { mb.Set(txn, "a", 1) })
		u1 := lazyRecord(t, b, func(txn *Transaction) { mb.Set(txn, "b", 2) })
		u2 := lazyRecord(t, b, func(txn *Transaction) { mb.Set(txn, "d", New(WithGUID("sub"))) })
		a := New(WithClientID(1))
		lazyApply(t, a, u0, v2)
		lazyApply(t, a, u2, v2)
		get(a, lazyMap)
		lazyApply(t, a, u1, v2)
		if g := a.GetSubdocGUIDs(); len(g) != 1 || g[0] != "sub" {
			t.Fatalf("subdocs = %v, want [sub]", g)
		}
		if v, _ := a.GetMap("m").Get("d"); v == nil {
			t.Fatal("subdoc entry missing")
		}
		if s := staleRootParents(a); len(s) > 0 {
			t.Fatalf("stale parents: %v", s)
		}
	})
}

// Skip structs from a merge of non-contiguous updates park what follows them.
func TestLazyRoot_ParkedAfterSkip(t *testing.T) {
	lazyVariants(t, func(t *testing.T, v2 bool, get func(*Doc, lazyKind)) {
		b := New(WithClientID(2))
		tb := b.GetText("t")
		u0 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 0, "a", nil) })
		u1 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 1, "b", nil) })
		u2 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 0, "c", nil) })
		merged, err := MergeUpdatesV1(u0, u2)
		if err != nil {
			t.Fatal(err)
		}
		a := New(WithClientID(1))
		lazyApply(t, a, merged, v2)
		if a.store.pending == nil {
			t.Fatal("struct after the skip did not park")
		}
		get(a, lazyText)
		lazyApply(t, a, u1, v2)
		lazyCheck(t, a, lazyText, lazyView(t, b, lazyText))
	})
}

// A local edit between first access and the filler keeps the parked struct.
func TestLazyRoot_LocalEditBeforeFiller(t *testing.T) {
	b := New(WithClientID(2))
	tb := b.GetText("t")
	u0 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 0, "a", nil) })
	u1 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 1, "b", nil) })
	u2 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 0, "c", nil) })
	a := New(WithClientID(1))
	lazyApply(t, a, u0, false)
	lazyApply(t, a, u2, false)
	ta := a.GetText("t")
	a.Transact(func(txn *Transaction) { ta.Insert(txn, 0, "d", nil) })
	lazyApply(t, a, u1, false)
	lazyApply(t, b, EncodeStateAsUpdateV1(a, b.StateVector()), false)
	if ta.ToString() != tb.ToString() || ta.ToString() != "dcab" {
		t.Fatalf("a=%q b=%q, want both \"dcab\"", ta.ToString(), tb.ToString())
	}
}

// Deletes that arrive before their targets resolve against the accessed type.
func TestLazyRoot_PendingDeletes(t *testing.T) {
	lazyVariants(t, func(t *testing.T, v2 bool, get func(*Doc, lazyKind)) {
		b := New(WithClientID(2))
		tb := b.GetText("t")
		u0 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 0, "a", nil) })
		u1 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 1, "b", nil) })
		u2 := lazyRecord(t, b, func(txn *Transaction) { tb.Insert(txn, 0, "cd", nil) })
		u3 := lazyRecord(t, b, func(txn *Transaction) { tb.Delete(txn, 1, 2) }) // delete-only: "d" and "a"
		a := New(WithClientID(1))
		lazyApply(t, a, u3, v2)
		lazyApply(t, a, u0, v2)
		lazyApply(t, a, u2, v2)
		get(a, lazyText)
		lazyApply(t, a, u1, v2)
		if len(a.store.pendingDs.clients) != 0 {
			t.Fatal("deletes still pending")
		}
		lazyCheck(t, a, lazyText, fmt.Sprint(tb.ToDelta()))
	})
}

// A move integrated before its cross-client target claims it once the
// target arrives after first access.
func TestLazyRoot_PendingMoveTarget(t *testing.T) {
	lazyVariants(t, func(t *testing.T, v2 bool, get func(*Doc, lazyKind)) {
		b := New(WithClientID(2))
		ab := b.GetArray("a")
		u0 := lazyRecord(t, b, func(txn *Transaction) { ab.Push(txn, []any{"x"}) })
		u1 := lazyRecord(t, b, func(txn *Transaction) { ab.Push(txn, []any{"y"}) })
		c := New(WithClientID(3))
		ac := c.GetArray("a")
		lazyApply(t, c, u0, false)
		lazyApply(t, c, u1, false)
		um := lazyRecord(t, c, func(txn *Transaction) { ac.Move(txn, 1, 0) })
		a := New(WithClientID(1))
		lazyApply(t, a, u0, v2)
		lazyApply(t, a, um, v2)
		get(a, lazyArray)
		lazyApply(t, a, u1, v2)
		lazyCheck(t, a, lazyArray, lazyView(t, c, lazyArray))
	})
}

// lazySeeds is the TestLazyRoot_Shuffled seed count; FUZZ_ITER overrides it.
func lazySeeds() int {
	if v := os.Getenv("FUZZ_ITER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return raceSeeds(400)
}

// lazyHistory builds a random concurrent multi-client history over every root
// kind and returns its updates in causal order.
func lazyHistory(t *testing.T, r *rand.Rand) [][]byte {
	const peers = 3
	docs := make([]*Doc, peers)
	seen := make([]int, peers) // updates each peer has applied, by index into log
	var log [][]byte
	var from []int
	for i := range docs {
		docs[i] = New(WithClientID(ClientID(i + 1)))
	}
	ops := 8 + r.Intn(16)
	for n := 0; n < ops; n++ {
		p := r.Intn(peers)
		d := docs[p]
		if r.Intn(3) == 0 {
			for ; seen[p] < len(log); seen[p]++ {
				if from[seen[p]] != p {
					lazyApply(t, d, log[seen[p]], false)
				}
			}
		}
		var u []byte
		un := d.OnUpdate(func(x []byte, _ any) { u = append([]byte(nil), x...) })
		d.Transact(func(txn *Transaction) { lazyRandomOp(r, txn) })
		un()
		if u != nil {
			log = append(log, u)
			from = append(from, p)
		}
	}
	return log
}

func lazyRandomOp(r *rand.Rand, txn *Transaction) {
	letters := "abcdefgh"
	s := string(letters[r.Intn(len(letters))])
	switch lazyKind(r.Intn(4)) {
	case lazyText:
		tx := txn.GetText("t")
		switch n := tx.Len(); {
		case n > 0 && r.Intn(3) == 0:
			i := r.Intn(n)
			tx.Delete(txn, i, 1+r.Intn(n-i))
		case n > 0 && r.Intn(4) == 0:
			tx.Format(txn, r.Intn(n), 1, Attributes{"b": true})
		default:
			tx.Insert(txn, r.Intn(n+1), s+s, nil)
		}
	case lazyArray:
		a := txn.GetArray("a")
		switch n := a.Len(); {
		case n > 1 && r.Intn(3) == 0:
			a.Move(txn, r.Intn(n), r.Intn(n))
		case n > 0 && r.Intn(3) == 0:
			a.Delete(txn, r.Intn(n), 1)
		default:
			a.Insert(txn, r.Intn(n+1), []any{s, r.Intn(9)})
		}
	case lazyMap:
		m := txn.GetMap("m")
		if r.Intn(4) == 0 {
			m.Delete(txn, s[:1])
		} else {
			m.Set(txn, string(letters[r.Intn(3)]), s)
		}
	default:
		f := txn.GetXmlFragment("x")
		if n := f.Len(); n > 0 && r.Intn(3) == 0 {
			f.Delete(txn, r.Intn(n), 1)
		} else {
			f.InsertElement(txn, r.Intn(n+1), NewYXmlElement(s))
		}
	}
}

// TestLazyRoot_Shuffled delivers random multi-client histories in shuffled
// order, optionally merged into skip-carrying chunks and in either encoding,
// to fresh docs that first access each root at a random point, and requires
// every one to match an eagerly accessed reference and reload identically.
func TestLazyRoot_Shuffled(t *testing.T) {
	first := 0
	if v, err := strconv.Atoi(os.Getenv("FUZZ_SEED")); err == nil {
		first = v
	}
	for seed := first; seed < first+lazySeeds(); seed++ {
		lazyShuffledSeed(t, seed)
	}
}

var lazyKinds = []lazyKind{lazyText, lazyArray, lazyMap, lazyXML}

func lazyShuffledSeed(t *testing.T, seed int) {
	defer func() {
		if t.Failed() {
			t.Logf("reproduce: FUZZ_SEED=%d FUZZ_ITER=1", seed)
		}
	}()
	r := rand.New(rand.NewSource(int64(seed)))
	log := lazyHistory(t, r)
	ref := New()
	for _, k := range lazyKinds {
		lazyGet(ref, nil, k)
	}
	for _, u := range log {
		lazyApply(t, ref, u, false)
	}
	want := make([]string, len(lazyKinds))
	for i, k := range lazyKinds {
		want[i] = lazyView(t, ref, k)
	}
	for recv := 0; recv < 4; recv++ {
		chunks := lazyChunks(t, r, log)
		at := make([]int, len(lazyKinds)) // chunks applied before first access; len+1 defers it to the final check
		for i := range at {
			at[i] = r.Intn(len(chunks) + 2)
		}
		v2 := make([]bool, len(chunks))
		for i := range v2 {
			v2[i] = r.Intn(2) == 0
		}
		d := lazyReceive(t, chunks, at, v2, r.Intn(2) == 0)
		for i, k := range lazyKinds {
			if got := lazyView(t, d, k); got != want[i] {
				t.Fatalf("recv %d root %q (first access after %d of %d chunks): got %s, want %s",
					recv, k.root(), at[i], len(chunks), got, want[i])
			}
			lazyCheck(t, d, k, want[i])
		}
	}
}

// lazyChunks shuffles log and merges random runs of it, so a chunk can carry
// skip structs for the clocks it withholds.
func lazyChunks(t *testing.T, r *rand.Rand, log [][]byte) [][]byte {
	order := r.Perm(len(log))
	var chunks [][]byte
	for i := 0; i < len(order); {
		var part [][]byte
		for n := 1 + r.Intn(3)*r.Intn(2); n > 0 && i < len(order); n, i = n-1, i+1 {
			part = append(part, log[order[i]])
		}
		c := part[0]
		if len(part) > 1 {
			var err error
			if c, err = MergeUpdatesV1(part...); err != nil {
				t.Fatal(err)
			}
		}
		chunks = append(chunks, c)
	}
	return chunks
}

// lazyReceive applies chunks to a fresh doc, first accessing root kind i just
// before chunk at[i] (or after the last one).
func lazyReceive(t *testing.T, chunks [][]byte, at []int, v2 []bool, viaTxn bool) *Doc {
	t.Helper()
	d := New(WithClientID(100))
	access := func(step int) {
		for i, k := range lazyKinds {
			if at[i] != step {
				continue
			}
			if viaTxn {
				d.Transact(func(txn *Transaction) { lazyGet(d, txn, k) })
			} else {
				lazyGet(d, nil, k)
			}
		}
	}
	for step, c := range chunks {
		access(step)
		lazyApply(t, d, c, v2[step])
	}
	access(len(chunks))
	if d.store.pending != nil || len(d.store.pendingDs.clients) > 0 {
		t.Fatal("updates still parked after full delivery")
	}
	return d
}
