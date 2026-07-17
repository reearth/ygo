package mobile

import (
	"bytes"
	"sync"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestDoc_ConstructionAndClientID(t *testing.T) {
	d := NewDoc()
	if id := d.ClientID(); id < 0 || id > (1<<53)-1 {
		t.Fatalf("NewDoc ClientID = %d, want within [0, 2^53 - 1]", id)
	}

	d2, err := NewDocWithClientID(42)
	if err != nil || d2.ClientID() != 42 {
		t.Fatalf("NewDocWithClientID(42): id=%d err=%v", d2.ClientID(), err)
	}

	// Boundary: 2^53 - 1 (Number.MAX_SAFE_INTEGER) is accepted; 2^53 is not.
	if d3, err := NewDocWithClientID((1 << 53) - 1); err != nil || d3.ClientID() != (1<<53)-1 {
		t.Fatalf("NewDocWithClientID(2^53-1): id=%d err=%v, want accepted", d3.ClientID(), err)
	}
	if _, err := NewDocWithClientID(1 << 53); err == nil {
		t.Fatal("NewDocWithClientID(2^53): expected error (one past max safe integer)")
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

func TestDoc_ReadAccessors(t *testing.T) {
	d := NewDoc()
	txt := d.d.GetText("t")
	mp := d.d.GetMap("m")
	arr := d.d.GetArray("a")
	d.d.Transact(func(tx *crdt.Transaction) {
		txt.Insert(tx, 0, "hi", nil)
		mp.Set(tx, "k", "v")
		arr.Push(tx, []any{int64(1), "two"})
	})
	// Format the inserted text bold so the delta carries an attribute.
	d.d.Transact(func(tx *crdt.Transaction) {
		txt.Format(tx, 0, 2, crdt.Attributes{"bold": true})
	})

	if got := d.GetText("t"); got != "hi" {
		t.Fatalf("GetText = %q, want %q", got, "hi")
	}
	mj, err := d.GetMapJSON("m")
	if err != nil || string(mj) != `{"k":"v"}` {
		t.Fatalf("GetMapJSON = %q err=%v", mj, err)
	}
	aj, err := d.GetArrayJSON("a")
	if err != nil || string(aj) != `[1,"two"]` {
		t.Fatalf("GetArrayJSON = %q err=%v", aj, err)
	}
	// GetTextJSON must return the idiomatic Yjs rich-text delta (insert op +
	// formatting attributes), NOT the plain-string JSON. This assertion fails if
	// formatting is dropped, because it references the bold attribute.
	tj, err := d.GetTextJSON("t")
	if err != nil {
		t.Fatalf("GetTextJSON err=%v", err)
	}
	if want := `[{"insert":"hi","attributes":{"bold":true}}]`; !jsonEqual(t, tj, []byte(want)) {
		t.Fatalf("GetTextJSON = %q, want %q", tj, want)
	}

	d.Close()
	if got := d.GetText("t"); got != "" {
		t.Fatalf("GetText after Close = %q, want empty", got)
	}
	if _, err := d.GetMapJSON("m"); err != ErrClosed {
		t.Fatalf("GetMapJSON after Close = %v, want ErrClosed", err)
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
	// State vectors must match...
	if !bytes.Equal(dst.EncodeStateVector(), src.EncodeStateVector()) {
		t.Fatal("state vectors differ after sync; did not converge")
	}
	// ...and the synced content must be readable.
	if got := dst.GetText("t"); got != "hi" {
		t.Fatalf("after sync GetText = %q, want hi", got)
	}

	// Full-state path also works.
	dst2 := NewDoc()
	if err := dst2.ApplyUpdate(src.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate(full): %v", err)
	}
	if !bytes.Equal(dst2.EncodeStateVector(), src.EncodeStateVector()) {
		t.Fatal("full-state sync did not converge")
	}
	if got := dst2.GetText("t"); got != "hi" {
		t.Fatalf("after full-state sync GetText = %q, want hi", got)
	}
}

func TestDoc_EncodeDiff_InvalidStateVector(t *testing.T) {
	if _, err := NewDoc().EncodeDiff([]byte{0xff, 0xff, 0xff}); err == nil {
		t.Fatal("EncodeDiff with garbage state vector: expected error")
	}
}

func TestDoc_GetTextJSON_AbsentRootIsEmptyArray(t *testing.T) {
	tj, err := NewDoc().GetTextJSON("nope")
	if err != nil || string(tj) != "[]" {
		t.Fatalf("GetTextJSON(absent) = %q err=%v, want []", tj, err)
	}
}

// TestDoc_ConcurrentCloseIsRaceFree hammers every operation from many goroutines
// while Close races them. Under -race this asserts the lifecycle guard fully
// synchronizes access to the inner pointer; functionally it asserts nothing
// panics and, once Close has completed, operations return the documented closed
// values.
func TestDoc_ConcurrentCloseIsRaceFree(t *testing.T) {
	d := NewDoc()
	upd := NewDoc().EncodeStateAsUpdate() // a valid (empty) peer update to apply

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.ApplyUpdate(upd)
			_ = d.EncodeStateAsUpdate()
			_ = d.EncodeStateVector()
			_, _ = d.EncodeDiff(d.EncodeStateVector())
			_ = d.GetText("t")
			_, _ = d.GetTextJSON("t")
			_ = d.ClientID()
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); d.Close() }() // races the operations above
	wg.Wait()

	// Close has completed (happens-before via wg): the doc is now closed and
	// operations must return the documented closed values without panicking.
	if err := d.ApplyUpdate(upd); err != ErrClosed {
		t.Fatalf("ApplyUpdate after Close = %v, want ErrClosed", err)
	}
	if got := d.EncodeStateAsUpdate(); got != nil {
		t.Fatalf("EncodeStateAsUpdate after Close = %v, want nil", got)
	}
	if id := d.ClientID(); id != 0 {
		t.Fatalf("ClientID after Close = %d, want 0", id)
	}
}
