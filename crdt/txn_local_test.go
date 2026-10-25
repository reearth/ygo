package crdt

import "testing"

// Transaction.Local is false for a transaction that applies a remote update,
// as Yjs's readUpdate runs transact(..., local=false); true otherwise.
func TestUnit_TxnLocal_FalseForAppliedUpdates(t *testing.T) {
	src := newTestDoc(1)
	txt := src.GetText("t")
	src.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hi", nil) })
	v1 := EncodeStateAsUpdateV1(src, nil)
	v2 := EncodeStateAsUpdateV2(src, nil)

	for _, tc := range []struct {
		name  string
		apply func(d *Doc)
		want  bool
	}{
		{"Transact", func(d *Doc) { d.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "x", nil) }) }, true},
		{"ApplyUpdate", func(d *Doc) { _ = d.ApplyUpdate(v1) }, false},
		{"ApplyUpdateV1", func(d *Doc) { _ = ApplyUpdateV1(d, v1, "remote") }, false},
		{"ApplyUpdateV2", func(d *Doc) { _ = ApplyUpdateV2(d, v2, "remote") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDoc(2)
			var got []bool
			off := d.OnAfterTransaction(func(txn *Transaction) { got = append(got, txn.Local) })
			defer off()
			tc.apply(d)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("txn.Local = %v, want [%v]", got, tc.want)
			}
		})
	}
}

// A default UndoManager captures local transactions only: applying a peer's
// update must not put the peer's edit on the undo stack.
func TestUnit_TxnLocal_UndoManagerIgnoresAppliedUpdates(t *testing.T) {
	peer := newTestDoc(1)
	ptxt := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { ptxt.Insert(txn, 0, "peer", nil) })

	d := newTestDoc(2)
	txt := d.GetText("t")
	um := NewUndoManager(d, []sharedType{txt})
	defer um.Destroy()

	if err := d.ApplyUpdate(EncodeStateAsUpdateV1(peer, nil)); err != nil {
		t.Fatal(err)
	}
	if n := um.UndoStackSize(); n != 0 {
		t.Fatalf("UndoStackSize = %d after a remote apply, want 0", n)
	}
	if um.Undo() || txt.ToString() != "peer" {
		t.Fatalf("Undo removed the peer's edit: %q", txt.ToString())
	}
}

// Applied updates still squash adjacent same-client string runs: squashRuns
// is gated on nothing but new items, not on Local, so a 50-keystroke history
// loads as one item rather than fifty.
func TestUnit_TxnLocal_AppliedUpdatesStillSquash(t *testing.T) {
	src := newTestDoc(1)
	stxt := src.GetText("t")
	for i := 0; i < 50; i++ {
		src.Transact(func(txn *Transaction) { stxt.Insert(txn, i, "a", nil) })
	}
	dst := newTestDoc(2)
	if err := dst.ApplyUpdate(EncodeStateAsUpdateV1(src, nil)); err != nil {
		t.Fatal(err)
	}
	if n := len(dst.store.clients[1]); n != 1 {
		t.Fatalf("receiver stores %d items for one contiguous run, want 1", n)
	}
	if got := dst.GetText("t").ToString(); got != stxt.ToString() {
		t.Fatalf("receiver text = %q, want %q", got, stxt.ToString())
	}
}
