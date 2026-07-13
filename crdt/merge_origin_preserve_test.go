package crdt

import "testing"

// TestMerge_PreservesOriginWhenParentOutsideSet guards a merge/diff re-encode
// bug: encodeItem/encodeItemV2 stripped a real content item's Origin when the
// origin item's Parent was nil — but a Parent can be nil simply because the
// item's anchor chain bottoms out on an item OUTSIDE the merge input set (it
// lives in the receiver's base), not because it is a GC placeholder. Stripping
// the origin then detaches the item, so the receiver integrates it at the type
// head → reordering / loss.
//
// Scenario: base = "A" (client 1). "B" after A (client 2, origin=A). "C" after
// B (client 4, origin=B). The delta {B, C} is merged/diffed and applied onto a
// receiver holding "A". In the merge set {B, C}, B's origin A is absent so
// B.Parent is nil; C's origin B is present-but-parentless, which used to strip
// C's origin. Applying the re-encoded delta must still yield "ABC".
func TestMerge_PreservesOriginWhenParentOutsideSet(t *testing.T) {
	d1 := New(WithClientID(1))
	t1 := d1.GetText("t")
	d1.Transact(func(txn *Transaction) { t1.Insert(txn, 0, "A", nil) })
	base := fullState(d1)

	d2 := New(WithClientID(2))
	if err := ApplyUpdateV1(d2, base, nil); err != nil {
		t.Fatalf("d2 seed: %v", err)
	}
	t2 := d2.GetText("t")
	d2.Transact(func(txn *Transaction) { t2.Insert(txn, 1, "B", nil) }) // "AB", B.origin=A

	d4 := New(WithClientID(4))
	if err := ApplyUpdateV1(d4, fullState(d2), nil); err != nil {
		t.Fatalf("d4 seed: %v", err)
	}
	t4 := d4.GetText("t")
	d4.Transact(func(txn *Transaction) { t4.Insert(txn, 2, "C", nil) }) // "ABC", C.origin=B (client 4)

	// Delta carrying just {B, C} (everything an A-only receiver is missing).
	lOnly := New()
	if err := ApplyUpdateV1(lOnly, base, nil); err != nil {
		t.Fatalf("lOnly seed: %v", err)
	}
	sv, err := DecodeStateVectorV1(EncodeStateVectorV1(lOnly))
	if err != nil {
		t.Fatalf("sv: %v", err)
	}
	deltaV1 := EncodeStateAsUpdateV1(d4, sv)
	deltaV2 := EncodeStateAsUpdateV2(d4, sv)

	const want = "ABC"

	// applyOnBase seeds a receiver with "A", applies u via applyFn, returns text.
	applyOnBase := func(applyFn func(*Doc, []byte, any) error, u []byte) string {
		r := New()
		if err := ApplyUpdateV1(r, base, nil); err != nil {
			t.Fatalf("receiver seed: %v", err)
		}
		if err := applyFn(r, u, nil); err != nil {
			t.Fatalf("apply: %v", err)
		}
		return r.GetText("t").ToString()
	}

	// Sanity: applying the raw deltas directly already converges.
	if got := applyOnBase(ApplyUpdateV1, deltaV1); got != want {
		t.Fatalf("raw V1 delta: got %q, want %q", got, want)
	}
	if got := applyOnBase(ApplyUpdateV2, deltaV2); got != want {
		t.Fatalf("raw V2 delta: got %q, want %q", got, want)
	}

	// The bug: MergeUpdates re-encodes and strips C's origin.
	mergedV1, err := MergeUpdatesV1(deltaV1)
	if err != nil {
		t.Fatalf("MergeUpdatesV1: %v", err)
	}
	if got := applyOnBase(ApplyUpdateV1, mergedV1); got != want {
		t.Fatalf("MergeUpdatesV1 re-encode: got %q, want %q (origin stripped)", got, want)
	}

	mergedV2, err := MergeUpdatesV2(deltaV2)
	if err != nil {
		t.Fatalf("MergeUpdatesV2: %v", err)
	}
	if got := applyOnBase(ApplyUpdateV2, mergedV2); got != want {
		t.Fatalf("MergeUpdatesV2 re-encode: got %q, want %q (origin stripped)", got, want)
	}
}
