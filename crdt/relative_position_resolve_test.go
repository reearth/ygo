package crdt_test

import (
	"testing"

	"github.com/reearth/ygo/crdt"
)

// TestToAbsolutePosition_TnameAnchorResolvesToEndOfType verifies Yjs-parity
// resolution semantics for a tname/null-item RelativePosition: assoc >= 0
// resolves to the END of the type (its length); assoc < 0 resolves to the start
// (index 0). Previously ygo always resolved a null-item anchor to 0, so an
// end-of-type cursor silently snapped to the start on resolve.
func TestToAbsolutePosition_TnameAnchorResolvesToEndOfType(t *testing.T) {
	d := crdt.New(crdt.WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "hello", nil) })
	wantLen := txt.Len() // 5 UTF-16 units

	pos, ok := crdt.ToAbsolutePosition(d, crdt.RelativePosition{Tname: "t", Assoc: 0})
	if !ok {
		t.Fatal("resolve tname/assoc>=0: ok=false")
	}
	if pos.Index != wantLen {
		t.Fatalf("tname/assoc>=0 index = %d, want %d (end of type)", pos.Index, wantLen)
	}

	neg, ok := crdt.ToAbsolutePosition(d, crdt.RelativePosition{Tname: "t", Assoc: -1})
	if !ok {
		t.Fatal("resolve tname/assoc<0: ok=false")
	}
	if neg.Index != 0 {
		t.Fatalf("tname/assoc<0 index = %d, want 0 (start of type)", neg.Index)
	}
}

// TestToAbsolutePosition_EndCursorRoundTrip is the real-world regression:
// CreateRelativePositionFromIndex encodes an end-of-text cursor (assoc >= 0) as
// a tname anchor, and resolving it must return the end index, not the start.
func TestToAbsolutePosition_EndCursorRoundTrip(t *testing.T) {
	d := crdt.New(crdt.WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "hello", nil) })
	end := txt.Len()

	rp := crdt.CreateRelativePositionFromIndex(txt, end, 0)
	if rp.Item != nil {
		t.Fatalf("expected a tname anchor for end-of-text with assoc>=0, got item anchor %+v", rp.Item)
	}
	pos, ok := crdt.ToAbsolutePosition(d, rp)
	if !ok || pos.Index != end {
		t.Fatalf("end cursor resolved to index %d (ok=%v), want %d", pos.Index, ok, end)
	}
}

// TestToAbsolutePosition_TnameAnchorUnknownType resolves to 0 for a type that
// does not exist yet (length 0), regardless of assoc.
func TestToAbsolutePosition_TnameAnchorUnknownType(t *testing.T) {
	d := crdt.New(crdt.WithClientID(1))
	for _, assoc := range []int{0, -1} {
		pos, ok := crdt.ToAbsolutePosition(d, crdt.RelativePosition{Tname: "missing", Assoc: assoc})
		if !ok {
			t.Fatalf("assoc=%d: ok=false", assoc)
		}
		if pos.Index != 0 {
			t.Fatalf("assoc=%d: unknown-type index = %d, want 0", assoc, pos.Index)
		}
	}
}
