package crdt

import (
	"github.com/reearth/ygo/encoding"

	"errors"
	"testing"
)

// Tests for the value limit a caller passes the decodes outside an apply:
// DocContainsUpdateV1Limit, DiffUpdateV1Limit and HeldStructsDifferV1Limit
// each decode the whole update into a scratch document.

// manyValues is an update of client 5 that inserts n numbers into the array
// "a" in one struct, so a decode of it decodes one struct and n values.
func manyValues(n int) []byte {
	writer := New(WithClientID(5))
	arr := writer.GetArray("a")
	vals := make([]any, n)
	for i := range vals {
		vals[i] = i
	}
	writer.Transact(func(txn *Transaction) { arr.Insert(txn, 0, vals) })
	return EncodeStateAsUpdateV1(writer, nil)
}

// limitedDecoders are the three scratch decoders, each with a value limit,
// over doc and update.
func limitedDecoders(doc *Doc, update []byte, client ClientID) map[string]func(values int) error {
	return map[string]func(values int) error{
		"DocContainsUpdateV1Limit": func(values int) error {
			_, err := DocContainsUpdateV1Limit(doc, update, values)
			return err
		},
		"DiffUpdateV1Limit": func(values int) error {
			_, err := DiffUpdateV1Limit(update, StateVector{}, values)
			return err
		},
		"HeldStructsDifferV1Limit": func(values int) error {
			_, err := HeldStructsDifferV1Limit(doc, update, client, 0, values)
			return err
		},
	}
}

// Each scratch decoder decodes an update within its caller's value limit,
// and stops past it with ErrValueLimit, counting as ApplyBudget.Decoded
// counts an apply: a struct and each value it carries. A limit below zero
// bounds nothing.
func TestUnit_ValueLimit_TheScratchDecodersStopPastIt(t *testing.T) {
	const values = 100
	update := manyValues(values)
	doc := New(WithClientID(1))
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatal(err)
	}
	for name, decode := range limitedDecoders(doc, update, 5) {
		t.Run(name, func(t *testing.T) {
			for _, limit := range []int{-1, 1 + values} {
				if err := decode(limit); err != nil {
					t.Fatalf("a decode of %d values with a limit of %d returned %v", 1+values, limit, err)
				}
			}
			if err := decode(values); !errors.Is(err, ErrValueLimit) || !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("a decode of %d values with a limit of %d returned %v, want ErrValueLimit", 1+values, values, err)
			}
		})
	}
}

// The limit counts each value inside a nested Any value and each delete
// range, as an apply's budget does, so one struct holding a deep value, and
// a frame of delete ranges alone, are bounded too.
func TestUnit_ValueLimit_CountsNestedValuesAndDeleteRanges(t *testing.T) {
	writer := New(WithClientID(5))
	arr := writer.GetArray("a")
	inner := make([]any, 50)
	for i := range inner {
		inner[i] = map[string]any{"k": i}
	}
	writer.Transact(func(txn *Transaction) { arr.Insert(txn, 0, []any{inner}) })
	nested := EncodeStateAsUpdateV1(writer, nil)
	// One struct, one array, 50 maps and 50 entries.
	if _, err := DiffUpdateV1Limit(nested, StateVector{}, 1+1+50+50); err != nil {
		t.Fatalf("the nested value within its limit returned %v", err)
	}
	if _, err := DiffUpdateV1Limit(nested, StateVector{}, 1+1+50+49); !errors.Is(err, ErrValueLimit) {
		t.Fatalf("the nested value past its limit returned %v, want ErrValueLimit", err)
	}

	ranges := make([]DeleteRange, 20)
	for i := range ranges {
		ranges[i] = DeleteRange{Clock: uint64(2 * i), Len: 1}
	}
	deletes := deletesOf(9, ranges)
	doc := New(WithClientID(1))
	if _, err := DocContainsUpdateV1Limit(doc, deletes, 20); err != nil {
		t.Fatalf("20 delete ranges within a limit of 20 returned %v", err)
	}
	if _, err := DocContainsUpdateV1Limit(doc, deletes, 19); !errors.Is(err, ErrValueLimit) {
		t.Fatalf("20 delete ranges past a limit of 19 returned %v, want ErrValueLimit", err)
	}
}

// deletesOf is a struct-free V1 update deleting ranges of client.
func deletesOf(client ClientID, ranges []DeleteRange) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // no struct groups
	enc.WriteVarUint(1) // one client in the delete set
	enc.WriteVarUint(uint64(client))
	enc.WriteVarUint(uint64(len(ranges)))
	for _, r := range ranges {
		enc.WriteVarUint(r.Clock)
		enc.WriteVarUint(r.Len)
	}
	return append([]byte(nil), enc.Bytes()...)
}
