package crdt

import (
	"bytes"
	"testing"
)

// These tests reproduce the three P0 CRDT-convergence defects identified in the
// 2026-06-09 architecture review. Each is written to FAIL against the current
// implementation and pass once the corresponding fix lands.
//
//   C-1: concurrent YMap writes lose a key depending on apply order
//   C-2: a missing rightOrigin integrates immediately instead of parking
//   C-3: ygo rejects its own full-state encode (parent-by-ID decode hard-fail)

// fullState returns doc's complete state as a V1 update.
func fullState(d *Doc) []byte { return EncodeStateAsUpdateV1(d, nil) }

// diffFrom returns the part of doc's state not described by base's state vector.
func diffFrom(doc, base *Doc) []byte {
	sv, err := DecodeStateVectorV1(EncodeStateVectorV1(base))
	if err != nil {
		panic(err)
	}
	return EncodeStateAsUpdateV1(doc, sv)
}

// ---------------------------------------------------------------------------
// C-1 — concurrent YMap writes must converge regardless of apply order, and a
// live key must never vanish after cross-sync.
// ---------------------------------------------------------------------------

func TestP0_C1_MapConvergesRegardlessOfOrder(t *testing.T) {
	base := New()

	// Three concurrent writes off the same (empty) base: two to key "x", one
	// to key "y". The "y" write is the unrelated update that interleaves.
	mk := func(id ClientID, key, val string) []byte {
		d := New(WithClientID(id))
		if err := ApplyUpdateV1(d, fullState(base), nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		m := d.GetMap("m")
		d.Transact(func(txn *Transaction) { m.Set(txn, key, val) })
		return diffFrom(d, base)
	}
	u1 := mk(1, "x", "a1")
	u2 := mk(2, "y", "b2")
	u3 := mk(3, "x", "c3")

	apply := func(updates ...[]byte) *Doc {
		d := New()
		for _, u := range updates {
			if err := ApplyUpdateV1(d, u, nil); err != nil {
				t.Fatalf("apply: %v", err)
			}
		}
		return d
	}

	r1 := apply(u1, u2, u3)
	r2 := apply(u3, u2, u1)

	get := func(d *Doc) string {
		v, ok := d.GetMap("m").Get("x")
		if !ok {
			return "<absent>"
		}
		return v.(string)
	}

	x1, x2 := get(r1), get(r2)
	if x1 != x2 {
		t.Fatalf("order-dependent map value: r1 x=%q, r2 x=%q (must converge)", x1, x2)
	}

	// Cross-sync the two receivers; the key must survive and stay equal.
	if err := ApplyUpdateV1(r1, fullState(r2), nil); err != nil {
		t.Fatalf("cross-sync r1<-r2: %v", err)
	}
	if err := ApplyUpdateV1(r2, fullState(r1), nil); err != nil {
		t.Fatalf("cross-sync r2<-r1: %v", err)
	}
	if g1, g2 := get(r1), get(r2); g1 != g2 || g1 == "<absent>" {
		t.Fatalf("after cross-sync key x diverged/vanished: r1=%q r2=%q", g1, g2)
	}
}

// ---------------------------------------------------------------------------
// C-2 — an item whose rightOrigin references a not-yet-integrated client must
// park until that client arrives, not integrate at the wrong position.
// ---------------------------------------------------------------------------

func TestP0_C2_MissingRightOriginParks(t *testing.T) {
	dA := New(WithClientID(1))
	tA := dA.GetText("t")
	dA.Transact(func(txn *Transaction) { tA.Insert(txn, 0, "12", nil) })
	uA := fullState(dA)

	dB := New(WithClientID(2))
	if err := ApplyUpdateV1(dB, uA, nil); err != nil {
		t.Fatalf("B seed: %v", err)
	}
	tB := dB.GetText("t")
	dB.Transact(func(txn *Transaction) { tB.Insert(txn, 1, "b", nil) }) // "1b2"
	uB := diffFrom(dB, dA)

	dC := New(WithClientID(3))
	if err := ApplyUpdateV1(dC, uA, nil); err != nil {
		t.Fatalf("C seed A: %v", err)
	}
	if err := ApplyUpdateV1(dC, uB, nil); err != nil {
		t.Fatalf("C seed B: %v", err)
	}
	tC := dC.GetText("t")
	// Capture C's state vector (A+B) BEFORE C's own insert, so uC is just C's op.
	svBeforeC := New()
	if err := ApplyUpdateV1(svBeforeC, uA, nil); err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdateV1(svBeforeC, uB, nil); err != nil {
		t.Fatal(err)
	}
	dC.Transact(func(txn *Transaction) { tC.Insert(txn, 1, "c", nil) })
	uC := diffFrom(dC, svBeforeC) // C's insert; its left/right origins reference B's item

	// Causal order (A, B, C) is the reference result.
	ref := New()
	for _, u := range [][]byte{uA, uB, uC} {
		if err := ApplyUpdateV1(ref, u, nil); err != nil {
			t.Fatalf("ref apply: %v", err)
		}
	}
	want := ref.GetText("t").ToString()

	// Out-of-order delivery: A, then C (depends on B via rightOrigin), then B.
	r := New()
	for _, u := range [][]byte{uA, uC, uB} {
		if err := ApplyUpdateV1(r, u, nil); err != nil {
			t.Fatalf("ooo apply: %v", err)
		}
	}
	got := r.GetText("t").ToString()

	if got != want {
		t.Fatalf("out-of-order delivery diverged: got %q, want %q (causal order)", got, want)
	}
}

// ---------------------------------------------------------------------------
// C-3 — a full-state encode where a lower-clientID peer wrote into a
// higher-clientID peer's nested type must decode/apply cleanly (the child's
// parent-by-ID reference must be queued, not hard-rejected).
// ---------------------------------------------------------------------------

func TestP0_C3_SelfEncodeReloads(t *testing.T) {
	// Client 200 creates an XML element; client 100 (after syncing) sets an
	// attribute on it. The attribute item (client 100) references the element
	// (client 200) as its parent-by-ID. EncodeStateAsUpdate sorts groups
	// ascending by client, so the attribute decodes before its parent.
	d200 := New(WithClientID(200))
	frag := d200.GetXmlFragment("f")
	el := NewYXmlElement("div")
	d200.Transact(func(txn *Transaction) { frag.InsertElement(txn, 0, el) })

	d100 := New(WithClientID(100))
	if err := ApplyUpdateV1(d100, fullState(d200), nil); err != nil {
		t.Fatalf("d100 seed: %v", err)
	}
	children := d100.GetXmlFragment("f").Children()
	if len(children) != 1 {
		t.Fatalf("d100 expected 1 child, got %d", len(children))
	}
	el100, ok := children[0].(*YXmlElement)
	if !ok {
		t.Fatalf("d100 child is not *YXmlElement: %T", children[0])
	}
	d100.Transact(func(txn *Transaction) { el100.SetAttribute(txn, "class", "hello") })

	// d100's full state contains both client groups (100 attr + 200 element).
	full := fullState(d100)

	fresh := New()
	if err := ApplyUpdateV1(fresh, full, nil); err != nil {
		t.Fatalf("applying own full-state encode failed: %v", err)
	}
	fc := fresh.GetXmlFragment("f").Children()
	if len(fc) != 1 {
		t.Fatalf("fresh expected 1 child, got %d", len(fc))
	}
	fel := fc[0].(*YXmlElement)
	if v, ok := fel.GetAttribute("class"); !ok || v != "hello" {
		t.Fatalf("fresh element attribute class=%q ok=%v, want \"hello\"", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Order-independence guard (down-payment on the review's #70 recommendation):
// a set of genuinely-concurrent updates must converge to byte-identical state
// regardless of the order in which a receiver applies them. This directly
// guards against regressions of C-1 (map LWW) and C-2 (rightOrigin parking).
// ---------------------------------------------------------------------------

func TestP0_ConcurrentUpdatesConvergeInAllOrders(t *testing.T) {
	base := New()

	// Each closure produces one independent update off the shared empty base,
	// using a distinct client ID. Mix of: same-key map writes (C-1), an
	// unrelated-key write (the interleaver), and concurrent text inserts at the
	// same index (C-2 territory once they reference each other on merge).
	makers := []func() []byte{
		func() []byte { return mapSet(t, base, 11, "x", "A") },
		func() []byte { return mapSet(t, base, 22, "y", "B") },
		func() []byte { return mapSet(t, base, 33, "x", "C") },
		func() []byte { return textIns(t, base, 44, 0, "Z") },
	}
	updates := make([][]byte, len(makers))
	for i, m := range makers {
		updates[i] = m()
	}

	// Reference convergence: apply in index order.
	ref := New()
	for _, u := range updates {
		if err := ApplyUpdateV1(ref, u, nil); err != nil {
			t.Fatalf("ref apply: %v", err)
		}
	}
	wantState := EncodeStateAsUpdateV1(ref, nil)
	wantText := ref.GetText("t").ToString()
	wantX, _ := ref.GetMap("m").Get("x")

	idx := make([]int, len(updates))
	for i := range idx {
		idx[i] = i
	}
	perms := 0
	permute(idx, 0, func(order []int) {
		perms++
		d := New()
		for _, i := range order {
			if err := ApplyUpdateV1(d, updates[i], nil); err != nil {
				t.Fatalf("perm %v apply: %v", order, err)
			}
		}
		// Same materialized content as the reference.
		if got := d.GetText("t").ToString(); got != wantText {
			t.Fatalf("order %v: text=%q want %q", order, got, wantText)
		}
		if gx, _ := d.GetMap("m").Get("x"); gx != wantX {
			t.Fatalf("order %v: map[x]=%v want %v", order, gx, wantX)
		}
		// And byte-identical full state (the strongest convergence check:
		// identical item set AND identical delete set).
		if got := EncodeStateAsUpdateV1(d, nil); !bytes.Equal(got, wantState) {
			t.Fatalf("order %v: full-state encode differs from reference (divergent CRDT state)", order)
		}
	})
	if perms != 24 {
		t.Fatalf("expected 24 permutations, ran %d", perms)
	}
}

// TestP0_ManyConcurrentSamePositionInserts_Converge stresses the YATA
// conflict-scan with a large conflict group: every insert lands at position 0 of
// the same empty base, so all share Origin=nil. It guards the #54-C allocation
// optimization (reusing the conflict map via clear() instead of reallocating it
// inside the scan loop) — the change must not alter the conflict-resolution
// outcome, so every apply order must converge to byte-identical state with all
// inserts surviving.
func TestP0_ManyConcurrentSamePositionInserts_Converge(t *testing.T) {
	const n = 24
	base := New()
	updates := make([][]byte, n)
	for i := 0; i < n; i++ {
		updates[i] = textIns(t, base, ClientID(i+1), 0, string(rune('a'+i)))
	}

	apply := func(order []int) *Doc {
		d := New()
		for _, i := range order {
			if err := ApplyUpdateV1(d, updates[i], nil); err != nil {
				t.Fatalf("apply: %v", err)
			}
		}
		return d
	}

	forward := make([]int, n)
	reverse := make([]int, n)
	rotated := make([]int, n)
	for i := 0; i < n; i++ {
		forward[i], reverse[i], rotated[i] = i, n-1-i, (i+n/2)%n
	}

	ref := apply(forward)
	wantState := EncodeStateAsUpdateV1(ref, nil)
	wantText := ref.GetText("t").ToString()
	if len([]rune(wantText)) != n {
		t.Fatalf("reference lost inserts: got %d chars, want %d (%q)", len([]rune(wantText)), n, wantText)
	}

	for _, tc := range []struct {
		name  string
		order []int
	}{{"reverse", reverse}, {"rotated", rotated}} {
		d := apply(tc.order)
		if got := d.GetText("t").ToString(); got != wantText {
			t.Fatalf("%s order: text=%q want %q", tc.name, got, wantText)
		}
		if got := EncodeStateAsUpdateV1(d, nil); !bytes.Equal(got, wantState) {
			t.Fatalf("%s order: full-state encode differs from reference (divergent CRDT state)", tc.name)
		}
	}
}

func mapSet(t *testing.T, base *Doc, id ClientID, key, val string) []byte {
	t.Helper()
	d := New(WithClientID(id))
	if err := ApplyUpdateV1(d, fullState(base), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := d.GetMap("m")
	d.Transact(func(txn *Transaction) { m.Set(txn, key, val) })
	return diffFrom(d, base)
}

func textIns(t *testing.T, base *Doc, id ClientID, idx int, s string) []byte {
	t.Helper()
	d := New(WithClientID(id))
	if err := ApplyUpdateV1(d, fullState(base), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tx := d.GetText("t")
	d.Transact(func(txn *Transaction) { tx.Insert(txn, idx, s, nil) })
	return diffFrom(d, base)
}

// permute calls fn with every permutation of a (restoring a between calls).
func permute(a []int, k int, fn func([]int)) {
	if k == len(a) {
		cp := make([]int, len(a))
		copy(cp, a)
		fn(cp)
		return
	}
	for i := k; i < len(a); i++ {
		a[k], a[i] = a[i], a[k]
		permute(a, k+1, fn)
		a[k], a[i] = a[i], a[k]
	}
}
