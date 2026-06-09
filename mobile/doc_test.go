package mobile

import "testing"

func TestDoc_ConstructionAndClientID(t *testing.T) {
	d := NewDoc()
	if d.ClientID() <= 0 {
		t.Fatalf("NewDoc ClientID = %d, want positive (random uint32)", d.ClientID())
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
}
