package crdt

import (
	"reflect"
	"testing"
)

// TestV2_OriginRightDeferral_ArrayConverges guards a V2 apply-path bug: an item
// whose OriginRight references a not-yet-integrated clock must be deferred, like
// the V1 path does (update.go C-2 guard), or it integrates at the wrong
// position. V2 decodes client groups descending, so a higher-clientID item can
// decode before its lower-clientID right neighbour.
//
// Setup: canonical [L, M, R] where L=client1, R=client2 (after L), M=client3
// (between L and R, so M.Origin=L, M.OriginRight=R). A target holding only L
// receives {R, M}. In V2 the group order is client 3 (M) before client 2 (R),
// so M decodes before its OriginRight R exists. ApplyUpdateV2 must match
// ApplyUpdateV1's correct [L, M, R]; before the fix it produced [L, R, M].
func TestV2_OriginRightDeferral_ArrayConverges(t *testing.T) {
	d1 := New(WithClientID(1))
	a1 := d1.GetArray("a")
	d1.Transact(func(txn *Transaction) { a1.Insert(txn, 0, []any{"L"}) })

	d2 := New(WithClientID(2))
	if err := ApplyUpdateV1(d2, fullState(d1), nil); err != nil {
		t.Fatalf("d2 seed: %v", err)
	}
	a2 := d2.GetArray("a")
	d2.Transact(func(txn *Transaction) { a2.Insert(txn, 1, []any{"R"}) }) // [L, R]

	d3 := New(WithClientID(3))
	if err := ApplyUpdateV1(d3, fullState(d1), nil); err != nil {
		t.Fatalf("d3 seed L: %v", err)
	}
	if err := ApplyUpdateV1(d3, fullState(d2), nil); err != nil {
		t.Fatalf("d3 seed R: %v", err)
	}
	a3 := d3.GetArray("a")
	d3.Transact(func(txn *Transaction) { a3.Insert(txn, 1, []any{"M"}) }) // [L, M, R]

	want := []any{"L", "M", "R"}
	if got := d3.GetArray("a").ToSlice(); !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical setup produced %v, want %v", got, want)
	}

	// State vector of a doc that holds only L → the {R, M} delta.
	lOnly := New()
	if err := ApplyUpdateV1(lOnly, fullState(d1), nil); err != nil {
		t.Fatalf("lOnly seed: %v", err)
	}
	sv, err := DecodeStateVectorV1(EncodeStateVectorV1(lOnly))
	if err != nil {
		t.Fatalf("sv: %v", err)
	}
	uV1 := EncodeStateAsUpdateV1(d3, sv)
	uV2 := EncodeStateAsUpdateV2(d3, sv)

	apply := func(applyFn func(*Doc, []byte, any) error, u []byte) []any {
		d := New(WithClientID(9))
		if err := ApplyUpdateV1(d, fullState(d1), nil); err != nil {
			t.Fatalf("target seed L: %v", err)
		}
		if err := applyFn(d, u, nil); err != nil {
			t.Fatalf("apply delta: %v", err)
		}
		return d.GetArray("a").ToSlice()
	}

	if got := apply(ApplyUpdateV1, uV1); !reflect.DeepEqual(got, want) {
		t.Fatalf("V1 apply: got %v, want %v", got, want)
	}
	if got := apply(ApplyUpdateV2, uV2); !reflect.DeepEqual(got, want) {
		t.Fatalf("V2 apply diverged from V1: got %v, want %v", got, want)
	}
}
