package crdt

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// unknownTypeRefUpdates returns a V1 and a V2 update of one struct, client 5's
// shared type with type ref 9 in the root map "m", which Yjs has no type for.
func unknownTypeRefUpdates(t *testing.T) (v1, v2 []byte) {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteVarUint(5)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireType | flagHasParentSub)
	enc.WriteUint8(1)
	enc.WriteVarString("m")
	enc.WriteVarString("k")
	enc.WriteUint8(9)
	enc.WriteVarUint(0)
	v1 = enc.Bytes()

	// The same struct with type ref 1 (a YMap), its type-ref column rewritten.
	src := New(WithClientID(5))
	m := src.GetMap("m")
	src.Transact(func(txn *Transaction) { m.Set(txn, "k", NewMapPrelim()) })
	valid := EncodeStateAsUpdateV2(src, nil)
	dec := encoding.NewDecoder(valid)
	out := encoding.NewEncoder()
	flag, err := dec.ReadVarUint()
	require.NoError(t, err)
	out.WriteVarUint(flag)
	for column := range 9 {
		b, err := dec.ReadVarBytes()
		require.NoError(t, err)
		if column == 7 {
			require.Equal(t, []byte{1}, b, "the type-ref column holds one YMap ref")
			b = []byte{9}
		}
		out.WriteVarBytes(b)
	}
	return v1, append(out.Bytes(), dec.RemainingBytes()...)
}

// A struct with a type ref Yjs has no type for is refused, as Yjs
// readContentType refuses it, and an update under the pending cap that
// carries it changes nothing.
func TestUnit_ApplyUpdate_RefusesAnUnknownTypeRefBeforeAnyChange(t *testing.T) {
	v1, v2 := unknownTypeRefUpdates(t)
	for _, tc := range []struct {
		name   string
		update []byte
		apply  func(*Doc, []byte, any) error
	}{{"V1", v1, ApplyUpdateV1}, {"V2", v2, ApplyUpdateV2}} {
		t.Run(tc.name, func(t *testing.T) {
			doc := New()
			before := EncodeStateAsUpdateV1(doc, nil)
			updates := observed(doc)
			err := tc.apply(doc, tc.update, nil)
			if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), "unknown type ref: 9") {
				t.Fatalf("err %v, want ErrInvalidUpdate naming the unknown type ref", err)
			}
			requireUnchanged(t, doc, before, "", updates)
			if _, ok := doc.GetMap("m").Get("k"); ok {
				t.Fatal("the map holds the refused struct")
			}
		})
	}
}

// Every pre-apply scanner reads a struct's type ref and refuses one Yjs has
// no type for, naming it as the decoder does.
func TestUnit_PendingScanners_RefuseAnUnknownTypeRef(t *testing.T) {
	v1, v2 := unknownTypeRefUpdates(t)
	want := func(t *testing.T, scanner string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "unknown type ref: 9") {
			t.Fatalf("%s: err %v, want the unknown type ref", scanner, err)
		}
	}

	var r pendingClientRange
	_, err := scanPendingGroupsV1(v1, StateVector{}, 0, &r, nil, 0, nil, false)
	want(t, "V1 frame", err)
	d, err := newV2Decoder(v2)
	require.NoError(t, err)
	_, err = scanPendingGroupsV2(d, StateVector{}, 0, &r, nil, 0, nil, false)
	want(t, "V2 frame", err)

	for _, update := range [][]byte{v1, v2} {
		isV2 := len(update) == len(v2) && string(update) == string(v2)
		var start *v2Decoder
		var rest encoding.Decoder
		if isV2 {
			dec, err := newV2Decoder(update)
			require.NoError(t, err)
			start, rest = dec, *dec.restDec
		}
		_, _, _, _, err := scanGroupClients(update, start, rest, nil)
		want(t, "group walk", err)

		lenient := newPendingScanner(update, isV2)
		lenient.lenient = true
		_, err = scanPendingGroupsBounded(lenient, StateVector{}, 0, nil, nil, nil, nil)
		want(t, "lenient scanner", scanReason(err))
	}
}
