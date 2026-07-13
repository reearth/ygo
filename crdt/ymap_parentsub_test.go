package crdt

import "testing"

// TestYMap_ParentSubInheritedOnDeferredParent guards a V1-decoder key-loss bug:
// when a map-keyed item's origin is authored by a higher-clientID peer, the item
// decodes BEFORE its origin's client group within the same update (client groups
// are written ascending), so it is deferred and resolved by
// resolveWithinUpdatePending. That retry must inherit ParentSub from the origin
// item — otherwise the item integrates keyless (as a plain sequence element),
// vanishes from itemMap, and the key is silently lost. The two sibling
// resolution sites (decodeItem, the doc-level drain) and the V2 decoder all
// inherit ParentSub; this V1 site was the one that missed it.
//
// Decisive shape: a single document's own full-state encode fails to round-trip
// through EncodeStateAsUpdateV1 -> ApplyUpdateV1.
func TestYMap_ParentSubInheritedOnDeferredParent(t *testing.T) {
	// Peer B (higher clientID) sets key "k". Resolve the map handle outside the
	// transaction (GetMap takes the doc lock, which Transact already holds).
	dB := New(WithClientID(2))
	mB := dB.GetMap("m")
	dB.Transact(func(txn *Transaction) { mB.Set(txn, "k", "fromB") })

	// Peer A (lower clientID) integrates B, then overwrites "k". A's item has
	// its origin/left set to B's item, so A's item references a higher-clientID
	// predecessor.
	dA := New(WithClientID(1))
	if err := ApplyUpdateV1(dA, EncodeStateAsUpdateV1(dB, nil), nil); err != nil {
		t.Fatalf("dA seed: %v", err)
	}
	mA := dA.GetMap("m")
	dA.Transact(func(txn *Transaction) { mA.Set(txn, "k", "fromA") })

	if v, ok := mA.Get("k"); !ok || v != "fromA" {
		t.Fatalf("precondition: dA should resolve k=fromA, got %v ok=%v", v, ok)
	}

	// A's full state contains both client groups (1 before 2). Item {1,0}
	// decodes before its origin {2,0}, exercising the deferred-parent retry.
	full := EncodeStateAsUpdateV1(dA, nil)
	fresh := New()
	if err := ApplyUpdateV1(fresh, full, nil); err != nil {
		t.Fatalf("applying own full-state encode failed: %v", err)
	}
	if v, ok := fresh.GetMap("m").Get("k"); !ok || v != "fromA" {
		t.Fatalf("key k lost/wrong after self round-trip: got %v ok=%v, want \"fromA\"", v, ok)
	}
}

// TestYMap_DeferredParentConvergesAcrossApplyOrder is the multi-peer symptom of
// the same bug: two receivers that integrate the identical set of updates in
// different orders must converge, and the map key must not vanish. Before the
// ParentSub-inheritance fix, whichever receiver decoded the keyed item before
// its origin's client group lost the key, so the two receivers diverged.
func TestYMap_DeferredParentConvergesAcrossApplyOrder(t *testing.T) {
	dB := New(WithClientID(2))
	mB := dB.GetMap("m")
	dB.Transact(func(txn *Transaction) { mB.Set(txn, "k", "fromB") })
	uB := EncodeStateAsUpdateV1(dB, nil)

	dA := New(WithClientID(1))
	if err := ApplyUpdateV1(dA, uB, nil); err != nil {
		t.Fatalf("dA seed: %v", err)
	}
	mA := dA.GetMap("m")
	dA.Transact(func(txn *Transaction) { mA.Set(txn, "k", "fromA") })
	uA := EncodeStateAsUpdateV1(dA, nil) // full state: contains both client groups

	apply := func(updates ...[]byte) (any, bool) {
		d := New()
		for _, u := range updates {
			if err := ApplyUpdateV1(d, u, nil); err != nil {
				t.Fatalf("apply: %v", err)
			}
		}
		return d.GetMap("m").Get("k")
	}

	vAB, okAB := apply(uB, uA)
	vBA, okBA := apply(uA, uB)
	if !okAB || !okBA {
		t.Fatalf("key k vanished: order(B,A) ok=%v order(A,B) ok=%v", okAB, okBA)
	}
	if vAB != vBA {
		t.Fatalf("order-dependent divergence: (B,A)=%v (A,B)=%v", vAB, vBA)
	}
	if vAB != "fromA" {
		t.Fatalf("wrong winner: got %v, want \"fromA\" (client 1's write supersedes client 2's origin)", vAB)
	}
}
