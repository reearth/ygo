package crdt

import (
	"errors"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// WithClientID panics at the call for a client ID a V2 update cannot carry,
// and accepts the largest one it can, whose writes encode in V2. A remote
// struct from such a client is refused, so no document holds a struct
// EncodeStateAsUpdateV2 cannot write.
func TestUnit_WithClientID_RefusesAClientPastWhatV2Carries(t *testing.T) {
	func() {
		defer func() {
			r := recover()
			msg, _ := r.(string)
			if !strings.Contains(msg, "WithClientID(36028797018963968)") {
				t.Fatalf("WithClientID(2^55) recovered %v, want its panic", r)
			}
		}()
		WithClientID(maxClient + 1)
		t.Fatal("WithClientID(2^55) returned")
	}()

	doc := New(WithClientID(maxClient))
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, "x", nil) })
	reader := New()
	if err := ApplyUpdateV2(reader, EncodeStateAsUpdateV2(doc, nil), nil); err != nil {
		t.Fatalf("the largest client's V2 update does not load: %v", err)
	}
	if got := reader.GetText("t").ToString(); got != "x" {
		t.Fatalf("the reader holds %q, want %q", got, "x")
	}

	// A V1 struct of client 2^55 inserting "x" into the root text "t".
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteVarUint(maxClient + 1)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireString)
	enc.WriteUint8(1)
	enc.WriteVarString("t")
	enc.WriteVarString("x")
	enc.WriteVarUint(0)
	remote := New()
	if err := ApplyUpdateV1(remote, enc.Bytes(), nil); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("a struct of client 2^55 was applied with err %v, want ErrInvalidUpdate", err)
	}
	if got := remote.StateVector().Clock(maxClient + 1); got != 0 {
		t.Fatalf("the document holds client 2^55 up to clock %d", got)
	}
	EncodeStateAsUpdateV2(remote, nil)
}
