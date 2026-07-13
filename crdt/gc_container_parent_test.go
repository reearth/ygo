package crdt

import "testing"

// TestGC_DeletedContainerParent_OrphanDrops guards against a merge-aborting bug:
// deleting a nested container (default gc:true) auto-GCs its ContentType into a
// ContentDeleted tombstone. A concurrent remote update that references that
// container by parent-ID must be treated as an orphan (parent=nil, item dropped
// from the visible tree) — matching Yjs — NOT rejected with a hard
// "parent item is not a ContentType" error that aborts the whole update/merge.
//
// Runs the scenario through both the V1 and V2 apply paths (decode + deferred
// resolve).
func TestGC_DeletedContainerParent_OrphanDrops(t *testing.T) {
	cases := []struct {
		name   string
		encode func(*Doc) []byte
		apply  func(*Doc, []byte, any) error
	}{
		{"V1", func(d *Doc) []byte { return EncodeStateAsUpdateV1(d, nil) }, ApplyUpdateV1},
		{"V2", func(d *Doc) []byte { return EncodeStateAsUpdateV2(d, nil) }, ApplyUpdateV2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := New(WithClientID(1))
			b := New(WithClientID(2))

			aFrag := a.GetXmlFragment("x")
			a.Transact(func(txn *Transaction) { aFrag.InsertElement(txn, 0, NewYXmlElement("div")) })
			if err := tc.apply(b, tc.encode(a), nil); err != nil {
				t.Fatalf("b seed: %v", err)
			}

			// A deletes the element; auto-GC tombstones the container's ContentType.
			a.Transact(func(txn *Transaction) { aFrag.Delete(txn, 0, 1) })

			// B, concurrently, inserts a child into the (still-alive to B) element.
			bElem := b.GetXmlFragment("x").Children()[0].(*YXmlElement)
			b.Transact(func(txn *Transaction) { bElem.InsertText(txn, 0, NewYXmlText()) })

			// Merge B's child-insert into A, which already GC'd the container.
			// Must orphan-drop, not abort.
			if err := tc.apply(a, tc.encode(b), nil); err != nil {
				t.Fatalf("merge B->A aborted instead of orphan-dropping: %v", err)
			}
			// Converge the other direction.
			if err := tc.apply(b, tc.encode(a), nil); err != nil {
				t.Fatalf("merge A->B: %v", err)
			}

			// Both peers converge; the element was deleted, so the fragment is
			// empty on both (the orphaned child is not visible under a deleted
			// container).
			ax, bx := a.GetXmlFragment("x").ToXML(), b.GetXmlFragment("x").ToXML()
			if ax != bx {
				t.Fatalf("diverged after cross-sync: A=%q B=%q", ax, bx)
			}
			if n := a.GetXmlFragment("x").Len(); n != 0 {
				t.Fatalf("expected empty fragment (element deleted), got len=%d xml=%q", n, ax)
			}
		})
	}
}
