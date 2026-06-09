package mobile

import (
	"bytes"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestDoc_ConstructionAndClientID(t *testing.T) {
	d := NewDoc()
	if id := d.ClientID(); id < 0 || id > (1<<53) {
		t.Fatalf("NewDoc ClientID = %d, want within [0, 2^53]", id)
	}

	d2, err := NewDocWithClientID(42)
	if err != nil || d2.ClientID() != 42 {
		t.Fatalf("NewDocWithClientID(42): id=%d err=%v", d2.ClientID(), err)
	}

	if _, err := NewDocWithClientID(-1); err == nil {
		t.Fatal("NewDocWithClientID(-1): expected error")
	}
	if _, err := NewDocWithClientID((1 << 53) + 1); err == nil {
		t.Fatal("NewDocWithClientID(2^53+1): expected error")
	}
}

func TestDoc_CloseIsIdempotentAndZeroAfter(t *testing.T) {
	d := NewDoc()
	d.Close()
	d.Close() // must not panic
	if got := d.ClientID(); got != 0 {
		t.Fatalf("ClientID after Close = %d, want 0", got)
	}
	if err := d.ApplyUpdate([]byte{1, 2, 3}); err != ErrClosed {
		t.Fatalf("ApplyUpdate after Close = %v, want ErrClosed", err)
	}
}

func TestDoc_SyncRoundTrip(t *testing.T) {
	// Producer doc with text "hi" (driven via the inner crdt.Doc — v1 has no mobile mutators).
	src := NewDoc()
	txt := src.d.GetText("t") // ref captured OUTSIDE Transact (deadlock-safe)
	src.d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "hi", nil) })

	// Receiver syncs via state-vector exchange.
	dst := NewDoc()
	diff, err := src.EncodeDiff(dst.EncodeStateVector())
	if err != nil {
		t.Fatalf("EncodeDiff: %v", err)
	}
	if err := dst.ApplyUpdate(diff); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	// Convergence check WITHOUT GetText (GetText arrives in Task 4): state vectors must match.
	if !bytes.Equal(dst.EncodeStateVector(), src.EncodeStateVector()) {
		t.Fatal("state vectors differ after sync; did not converge")
	}

	// Full-state path also works.
	dst2 := NewDoc()
	if err := dst2.ApplyUpdate(src.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate(full): %v", err)
	}
	if !bytes.Equal(dst2.EncodeStateVector(), src.EncodeStateVector()) {
		t.Fatal("full-state sync did not converge")
	}
}

func TestDoc_EncodeDiff_InvalidStateVector(t *testing.T) {
	if _, err := NewDoc().EncodeDiff([]byte{0xff, 0xff, 0xff}); err == nil {
		t.Fatal("EncodeDiff with garbage state vector: expected error")
	}
}
