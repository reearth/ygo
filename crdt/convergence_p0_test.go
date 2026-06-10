package crdt

import "testing"

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
