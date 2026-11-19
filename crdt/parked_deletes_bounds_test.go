package crdt

import (
	"errors"
	"strings"
	"testing"
)

// Tests for what bounds the pending deletes and the pending queue apart from
// the struct cap: ApplyBudget.MayParkDeletes judges a delete range where it
// parks, WithMaxPendingDeleteRanges caps the parked ranges on their own,
// the record of who parked a range keeps only what each apply newly parked,
// and PendingStats reports the content bytes the queue holds.

// mayParkOnly is an apply budget bounding nothing but where delete ranges
// park: only under client may, and asked records each client it is asked of.
func mayParkOnly(may ClientID, asked *[]ClientID) func(any) ApplyBudget {
	return func(any) ApplyBudget {
		return ApplyBudget{Decoded: -1, Structs: -1, Bytes: -1, Split: -1, MayParkDeletes: func(c ClientID) bool {
			*asked = append(*asked, c)
			return c == may
		}}
	}
}

// An apply that would park a delete range under a client MayParkDeletes
// answers false for is refused whole, with ErrParkedDeleteRefused, and leaves
// the document as it was, its own structs included; one under a client it
// answers true for parks; and an apply that parks nothing never asks.
func TestUnit_ApplyBudget_MayParkDeletesJudgesARangeWhereItParks(t *testing.T) {
	for _, f := range applyFormats {
		t.Run(f.name, func(t *testing.T) {
			doc := budgetDoc(t, "hello")
			var asked []ClientID
			doc.SetApplyBudget(mayParkOnly(2, &asked))

			if err := applyIn(t, doc, f.v2, editUpdate(t, "hello", func(txn *Transaction, text *YText) {
				text.Delete(txn, 0, 1)
				text.Insert(txn, 0, "j", nil)
			})); err != nil {
				t.Fatalf("an edit of held clocks was refused: %v", err)
			}
			if len(asked) != 0 {
				t.Fatalf("an apply that parks nothing asked MayParkDeletes of %v", asked)
			}

			before := EncodeStateAsUpdateV1(doc, nil)
			withStructs, err := MergeUpdatesV1(
				editUpdateFrom(t, "hello", 3, func(txn *Transaction, text *YText) { text.Insert(txn, 0, "x", nil) }),
				deleteSetUpdate(map[ClientID][]DeleteRange{9: {{Clock: 0, Len: 3}}}))
			if err != nil {
				t.Fatal(err)
			}
			for name, update := range map[string][]byte{
				"a delete of a client the document lacks": deleteSetUpdate(map[ClientID][]DeleteRange{9: {{Clock: 0, Len: 3}}}),
				"with structs of its own":                 withStructs,
			} {
				asked = nil
				err := applyIn(t, doc, f.v2, update)
				var refused *RefusedUpdateError
				if !errors.Is(err, ErrParkedDeleteRefused) || !errors.As(err, &refused) {
					t.Fatalf("%s returned %v, want a RefusedUpdateError wrapping ErrParkedDeleteRefused", name, err)
				}
				if len(asked) != 1 || asked[0] != 9 {
					t.Fatalf("%s asked MayParkDeletes of %v, want [9]", name, asked)
				}
				if got := EncodeStateAsUpdateV1(doc, nil); string(got) != string(before) {
					t.Fatalf("%s left the document changed", name)
				}
				if got := doc.PendingStats().DeleteRanges; got != 0 {
					t.Fatalf("%s left %d delete ranges parked", name, got)
				}
			}

			asked = nil
			if err := applyIn(t, doc, f.v2, deleteSetUpdate(map[ClientID][]DeleteRange{2: {{Clock: 50, Len: 3}}})); err != nil {
				t.Fatalf("a delete under a client that may park was refused: %v", err)
			}
			if len(asked) != 1 || asked[0] != 2 || doc.PendingStats().DeleteRanges != 1 {
				t.Fatalf("a delete under a client that may park asked %v and parked %d ranges, want [2] and 1", asked, doc.PendingStats().DeleteRanges)
			}
		})
	}
}

// WithMaxPendingDeleteRanges caps the parked delete ranges apart from the
// struct cap: an apply that would grow them past it is refused with
// ErrInvalidUpdate before anything changes, a resend of ranges already parked
// is not, and parked structs are not counted against it.
func TestUnit_MaxPendingDeleteRanges_CapsParkedRangesApartFromItems(t *testing.T) {
	for _, f := range applyFormats {
		t.Run(f.name, func(t *testing.T) {
			doc := New(WithClientID(1), WithMaxPendingDeleteRanges(2))
			three := deleteSetUpdate(map[ClientID][]DeleteRange{9: {{Clock: 0, Len: 1}, {Clock: 2, Len: 1}, {Clock: 4, Len: 1}}})
			if err := applyIn(t, doc, f.v2, three); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("an apply parking three ranges past a cap of two returned %v, want ErrInvalidUpdate", err)
			}
			two := deleteSetUpdate(map[ClientID][]DeleteRange{9: {{Clock: 0, Len: 1}, {Clock: 2, Len: 1}}})
			for _, name := range []string{"two ranges", "the same two sent again"} {
				if err := applyIn(t, doc, f.v2, two); err != nil {
					t.Fatalf("%s within the cap were refused: %v", name, err)
				}
				if got := doc.PendingStats().DeleteRanges; got != 2 {
					t.Fatalf("after %s the document parks %d ranges, want 2", name, got)
				}
			}
			one := deleteSetUpdate(map[ClientID][]DeleteRange{8: {{Clock: 0, Len: 1}}})
			if err := applyIn(t, doc, f.v2, one); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("a third range past the cap returned %v, want ErrInvalidUpdate", err)
			}

			writer := New(WithClientID(5))
			text := writer.GetText("t")
			var gapped [][]byte
			for i, s := range []string{"a", "b", "c", "d"} {
				sv := writer.StateVector()
				writer.Transact(func(txn *Transaction) { text.Insert(txn, i, s, nil) })
				gapped = append(gapped, EncodeStateAsUpdateV1(writer, sv))
			}
			for _, u := range gapped[1:] {
				if err := applyIn(t, doc, f.v2, u); err != nil {
					t.Fatalf("a struct that parks was refused by the delete-range cap: %v", err)
				}
			}
			if got := doc.PendingStats().Items; got == 0 {
				t.Fatal("the document parks no struct")
			}

			doc.SetMaxPendingDeleteRanges(3)
			if err := applyIn(t, doc, f.v2, one); err != nil {
				t.Fatalf("a range within the raised cap was refused: %v", err)
			}
		})
	}
}

// recordedRanges is how many delete ranges the document's record of who
// parked them holds, over all its records.
func recordedRanges(doc *Doc) int {
	n := 0
	for _, by := range doc.parkedDsBy {
		for _, ranges := range by.ds.clients {
			n += len(ranges)
		}
	}
	return n
}

// On a document that discards refused drains, the record of the origin of
// each parked delete range keeps only what each apply newly parked: a resend
// records nothing, and an apply that parks some new ranges records those.
// The record is pruned by the ranges it holds, so records of many ranges the
// queue no longer holds go even when they are few.
func TestUnit_DiscardRefusedDrain_TheRecordOfParkedDeletesKeepsWhatEachApplyNewlyParked(t *testing.T) {
	doc := New(WithClientID(1))
	doc.SetDiscardRefusedDrain(true)
	park := deleteSetUpdate(map[ClientID][]DeleteRange{9: {{Clock: 0, Len: 2}, {Clock: 4, Len: 2}}})
	for range 50 {
		if err := ApplyUpdateV1(doc, park, "a"); err != nil {
			t.Fatal(err)
		}
	}
	if got := recordedRanges(doc); got != 2 || len(doc.parkedDsBy) != 1 {
		t.Fatalf("50 sends of one frame are recorded as %d records of %d ranges, want 1 of 2", len(doc.parkedDsBy), got)
	}
	if err := ApplyUpdateV1(doc, deleteSetUpdate(map[ClientID][]DeleteRange{9: {{Clock: 0, Len: 3}, {Clock: 8, Len: 1}}}), "b"); err != nil {
		t.Fatal(err)
	}
	if got := recordedRanges(doc); got != 4 {
		t.Fatalf("a frame parking two new parts records %d ranges in all, want 4", got)
	}
	last := doc.parkedDsBy[len(doc.parkedDsBy)-1]
	if last.origin != "b" || len(last.ds.clients[9]) != 2 || last.ds.clients[9][0] != (DeleteRange{Clock: 2, Len: 1}) || last.ds.clients[9][1] != (DeleteRange{Clock: 8, Len: 1}) {
		t.Fatalf("the frame's record is %+v, want b's new parts [2,3) and [8,9)", last)
	}

	pruned := New(WithClientID(1))
	pruned.SetDiscardRefusedDrain(true)
	for k := range 5 {
		ranges := make([]DeleteRange, 10)
		for j := range ranges {
			ranges[j] = DeleteRange{Clock: uint64(20*k + 2*j), Len: 1}
		}
		if err := ApplyUpdateV1(pruned, deleteSetUpdate(map[ClientID][]DeleteRange{9: ranges}), k); err != nil {
			t.Fatal(err)
		}
	}
	if err := ApplyUpdateV1(pruned, textState(9, strings.Repeat("x", 90)), "fill"); err != nil {
		t.Fatal(err)
	}
	if got := pruned.PendingStats().DeleteRanges; got != 5 {
		t.Fatalf("the fill leaves %d ranges parked, want 5", got)
	}
	if got := recordedRanges(pruned); got > 2*5+16 {
		t.Fatalf("the record holds %d ranges of %d records for 5 ranges parked, want at most %d", got, len(pruned.parkedDsBy), 2*5+16)
	}
}

// PendingStats.ContentBytes is the content bytes the pending queue holds
// now, as ApplyBudget.Bytes counts a struct's content: it grows with what
// parks, holds still for a resend that merges into the queue, and falls to
// zero when the drain lands the queue.
func TestUnit_PendingStats_ContentBytesIsWhatTheQueueHoldsNow(t *testing.T) {
	for _, f := range applyFormats {
		t.Run(f.name, func(t *testing.T) {
			writer := New(WithClientID(5))
			text := writer.GetText("t")
			insert := func(at int, s string) []byte {
				sv := writer.StateVector()
				writer.Transact(func(txn *Transaction) { text.Insert(txn, at, s, nil) })
				return EncodeStateAsUpdateV1(writer, sv)
			}
			first, second, third := insert(0, "ab"), insert(2, "cdef"), insert(6, "gh")
			m := writer.GetMap("m")
			sv := writer.StateVector()
			writer.Transact(func(txn *Transaction) { m.Set(txn, "key", "value") })
			keyed := EncodeStateAsUpdateV1(writer, sv)

			doc := New(WithClientID(1))
			for _, step := range []struct {
				name   string
				update []byte
				want   int
			}{
				{"a frame behind a gap", second, 4},
				{"the same frame again", second, 4},
				{"the frame past it", third, 6},
				{"a map entry past it", keyed, 6 + len("value")},
				{"the fill", first, 0},
			} {
				if err := applyIn(t, doc, f.v2, step.update); err != nil {
					t.Fatalf("%s was refused: %v", step.name, err)
				}
				if got := doc.PendingStats().ContentBytes; got != step.want {
					t.Fatalf("after %s the queue holds %d content bytes, want %d", step.name, got, step.want)
				}
			}
		})
	}
}
