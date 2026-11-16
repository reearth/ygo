package crdt

import (
	"bytes"
	"errors"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// parkedStructs is an update of n one-character structs for client 2 at
// clocks 1 to n, each naming the one before it as its left origin. Clock 0 is
// never sent, so a document that has not seen it parks every one of them.
func parkedStructs(n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(2) // client
	enc.WriteVarUint(1) // first clock: clock 0 is the gap
	for i := range n {
		enc.WriteUint8(wireString | flagHasOrigin)
		enc.WriteVarUint(2)         // the left origin's client
		enc.WriteVarUint(uint64(i)) // its clock, the struct before this one
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// gapStruct is the struct parkedStructs leaves out: client 2's struct at
// clock 0, the first item of the root text "t".
func gapStruct() []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)        // clients
	enc.WriteVarUint(1)        // structs
	enc.WriteVarUint(2)        // client
	enc.WriteVarUint(0)        // clock
	enc.WriteUint8(wireString) // no origin, so the parent follows
	enc.WriteVarUint(1)        // a root, by name
	enc.WriteVarString("t")
	enc.WriteVarString("x")
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// pendingState is everything an update refused by the pending cap must leave
// as it was: the document's whole encoded state, what it holds parked, and
// how many structs its store holds.
type pendingState struct {
	state   string
	items   int
	ranges  int
	structs int
}

func pendingStateOf(doc *Doc) pendingState {
	stats := doc.PendingStats()
	return pendingState{
		state:   string(EncodeStateAsUpdateV1(doc, nil)),
		items:   stats.Items,
		ranges:  stats.DeleteRanges,
		structs: storeStructs(doc),
	}
}

// storeStructs is how many structs the document's store holds, every client's
// together.
func storeStructs(doc *Doc) int {
	n := 0
	for _, items := range doc.store.clients {
		n += len(items)
	}
	return n
}

// The pending cap refuses with ErrPendingCap, which is not the refusal
// unreadable bytes get, and which still wraps ErrInvalidUpdate. The two say
// different things about the sender: bytes that do not decode come from a
// broken sender, while an update over the cap only waits on clocks the
// document does not hold yet, and applies once the document holds them.
func TestUnit_PendingCap_IsItsOwnRefusalAndStillAnInvalidUpdate(t *testing.T) {
	const limit = 5

	t.Run("structs past the cap", func(t *testing.T) {
		doc := New(WithClientID(7), WithMaxPendingItems(limit))
		if err := ApplyUpdateV1(doc, parkedStructs(limit), nil); err != nil {
			t.Fatalf("%d structs, the cap, were refused: %v", limit, err)
		}
		before := pendingStateOf(doc)
		if before.items != limit {
			t.Fatalf("%d structs are parked, want the %d the cap holds", before.items, limit)
		}

		err := ApplyUpdateV1(doc, parkedStructs(4*limit), nil)
		if !errors.Is(err, ErrPendingCap) {
			t.Fatalf("the update was refused with %v, want ErrPendingCap", err)
		}
		if !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("ErrPendingCap does not wrap ErrInvalidUpdate: %v", err)
		}
		if after := pendingStateOf(doc); after != before {
			t.Fatalf("the refused update changed the document:\nbefore %+v\nafter  %+v", before, after)
		}

		// The refusal only waits on clock 0: once the document holds it, the
		// same update applies and nothing is left parked.
		if err := ApplyUpdateV1(doc, gapStruct(), nil); err != nil {
			t.Fatalf("the struct at clock 0 was refused: %v", err)
		}
		if err := ApplyUpdateV1(doc, parkedStructs(4*limit), nil); err != nil {
			t.Fatalf("the update was refused again once the document held clock 0: %v", err)
		}
		if got := doc.StateVector().Clock(2); got != 4*limit+1 {
			t.Fatalf("client 2's clock is %d, want %d", got, 4*limit+1)
		}
		if items := doc.PendingStats().Items; items != 0 {
			t.Fatalf("%d structs are still parked", items)
		}
	})

	t.Run("delete ranges past the cap", func(t *testing.T) {
		doc := New(WithClientID(7), WithMaxPendingItems(limit))
		if err := ApplyUpdateV1(doc, parkedDeletes(98, limit), nil); err != nil {
			t.Fatalf("%d ranges, the cap, were refused: %v", limit, err)
		}
		before := pendingStateOf(doc)
		if before.ranges != limit {
			t.Fatalf("%d delete ranges are parked, want the %d the cap holds", before.ranges, limit)
		}

		err := ApplyUpdateV1(doc, parkedDeletes(99, limit+1), nil)
		if !errors.Is(err, ErrPendingCap) {
			t.Fatalf("the delete set was refused with %v, want ErrPendingCap", err)
		}
		if !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("ErrPendingCap does not wrap ErrInvalidUpdate: %v", err)
		}
		if after := pendingStateOf(doc); after != before {
			t.Fatalf("the refused delete set changed the document:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("bytes that do not read are not the cap", func(t *testing.T) {
		// Each of these carries too little to park anything, so what refuses
		// them can only be that they do not read. A frame that declares more
		// structs than the room and is cut after them is refused by the cap
		// first, and rightly: as far as it reads, it would park past it.
		one := parkedStructs(1)
		for name, update := range map[string][]byte{
			"a frame cut at its header": one[:1],
			"a struct cut short":        one[:len(one)-2],
		} {
			t.Run(name, func(t *testing.T) {
				doc := New(WithClientID(7), WithMaxPendingItems(limit))
				err := ApplyUpdateV1(doc, update, nil)
				if !errors.Is(err, ErrInvalidUpdate) {
					t.Fatalf("the malformed update was applied with err %v", err)
				}
				if errors.Is(err, ErrPendingCap) {
					t.Fatalf("a malformed update was refused as the pending cap: %v", err)
				}
			})
		}
	})
}
