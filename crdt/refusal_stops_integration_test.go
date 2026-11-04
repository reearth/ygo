package crdt

import (
	"encoding/hex"
	"errors"
	"testing"
)

// A struct Yjs refuses stops the update's integration where Yjs stops it: the
// struct of client 2 that continues its GC struct comes first in Yjs's order,
// so client 1's "x" after it stays out, in V1 and V2. The expected state is
// what Yjs 13.6.30 and 13.6.32 hold after the same updates.
func TestUnit_ApplyUpdate_ARefusalStopsTheIntegrationInYjsOrder(t *testing.T) {
	setup, err := hex.DecodeString("01010200000100") // client 2's GC struct at clock 0
	if err != nil {
		t.Fatal(err)
	}
	const want = "0101020000010102010001"
	for _, f := range []struct {
		name   string
		update string
		apply  func(*Doc, []byte, any) error
	}{
		{"V1", "020102000401017402616201010004010174017800", ApplyUpdateV1},
		{"V2", "0000020201000001040a0574616274780102410001010000020100010000", ApplyUpdateV2},
	} {
		t.Run(f.name, func(t *testing.T) {
			update, err := hex.DecodeString(f.update)
			if err != nil {
				t.Fatal(err)
			}
			doc := New(WithClientID(9))
			if err := ApplyUpdateV1(doc, setup, nil); err != nil {
				t.Fatal(err)
			}
			if err := f.apply(doc, update, nil); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("err %v, want ErrInvalidUpdate", err)
			}
			if got := doc.GetText("t").ToString(); got != "" {
				t.Fatalf("text %q, want \"\": Yjs integrates nothing after the refused struct", got)
			}
			if got := hex.EncodeToString(EncodeStateAsUpdateV1(doc, nil)); got != want {
				t.Fatalf("state %s, want Yjs's %s", got, want)
			}
		})
	}
}
