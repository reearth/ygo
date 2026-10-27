package crdt

import (
	"errors"
	"maps"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"
)

// deleteSetUpdate is a V1 update of no structs whose delete set names, for each
// client, its ranges.
func deleteSetUpdate(sets map[ClientID][]DeleteRange) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // clients
	enc.WriteVarUint(uint64(len(sets)))
	for _, client := range slices.Sorted(maps.Keys(sets)) {
		enc.WriteVarUint(uint64(client))
		enc.WriteVarUint(uint64(len(sets[client])))
		for _, r := range sets[client] {
			enc.WriteVarUint(r.Clock)
			enc.WriteVarUint(r.Len)
		}
	}
	return append([]byte(nil), enc.Bytes()...)
}

// parkedDeletes is a V1 update whose delete set names n one-clock ranges of
// client, every other clock from 0, which a document that never heard from
// client parks, every range.
func parkedDeletes(client ClientID, n int) []byte {
	ranges := make([]DeleteRange, n)
	for i := range n {
		ranges[i] = DeleteRange{Clock: uint64(2 * i), Len: 1}
	}
	return deleteSetUpdate(map[ClientID][]DeleteRange{client: ranges})
}

func applyIn(t *testing.T, doc *Doc, v2 bool, update []byte) error {
	t.Helper()
	if !v2 {
		return ApplyUpdateV1(doc, update, nil)
	}
	converted, err := UpdateV1ToV2(update)
	if err != nil {
		t.Fatal(err)
	}
	return ApplyUpdateV2(doc, converted, nil)
}

var applyFormats = []struct {
	name string
	v2   bool
}{{"V1", false}, {"V2", true}}

// textState is the full V1 state of a document whose text "t" holds s, written
// by client.
func textState(client ClientID, s string) []byte {
	doc := New(WithClientID(client))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, s, nil) })
	return EncodeStateAsUpdateV1(doc, nil)
}

// A keystroke costs the same whether or not 100,000 delete ranges of a client
// nobody holds are parked: a client's parked ranges are retried only once its
// structs have moved past the lowest of them. Before, every remote apply
// retried every parked range, copying each into a new set.
func TestAKeystrokeCostsNothingForParkedDeleteRanges(t *testing.T) {
	const n = 100_000
	state := textState(7, strings.Repeat("a", 8))
	keystroke := typedKeystroke(t, state)
	var allocated [2]uint64
	for i, parked := range [][]byte{nil, parkedDeletes(98, n)} {
		doc := New()
		if err := ApplyUpdateV1(doc, state, nil); err != nil {
			t.Fatal(err)
		}
		if parked != nil {
			if err := ApplyUpdateV1(doc, parked, nil); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := doc.PendingStats().DeleteRanges, i*n; got != want {
			t.Fatalf("%d delete ranges are parked, want %d", got, want)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		if err := ApplyUpdateV1(doc, keystroke, nil); err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		allocated[i] = after.TotalAlloc - before.TotalAlloc
		if got, want := doc.PendingStats().DeleteRanges, i*n; got != want {
			t.Fatalf("after the keystroke %d delete ranges are parked, want %d", got, want)
		}
		t.Logf("%d delete ranges parked: allocated %d bytes in %s", i*n, allocated[i], took)
	}
	if without, with := allocated[0], allocated[1]; with > without+64<<10 {
		t.Fatalf("with %d delete ranges parked the keystroke allocated %d bytes, %d with none, want within %d of it",
			n, with, without, 64<<10)
	}
}

// typedKeystroke is the update of one character a peer of state types into its
// text.
func typedKeystroke(t *testing.T, state []byte) []byte {
	t.Helper()
	peer := New(WithClientID(11))
	if err := ApplyUpdateV1(peer, state, nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	txt := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "k", nil) })
	return EncodeStateAsUpdateV1(peer, sv)
}

// Parked delete ranges are capped as parked items are (WithMaxPendingItems):
// an update whose delete set would grow the parked ranges past the cap is
// refused with ErrInvalidUpdate before it parks anything. An update that adds
// no range is never refused.
func TestParkedDeleteRangesAreCapped(t *testing.T) {
	const limit = 10
	for _, format := range applyFormats {
		t.Run(format.name, func(t *testing.T) {
			doc := New(WithMaxPendingItems(limit))
			parked := func() int { return doc.PendingStats().DeleteRanges }
			if err := applyIn(t, doc, format.v2, parkedDeletes(98, limit)); err != nil {
				t.Fatalf("%d ranges, the cap, were refused: %v", limit, err)
			}
			if got := parked(); got != limit {
				t.Fatalf("%d delete ranges are parked, want %d", got, limit)
			}
			if err := applyIn(t, doc, format.v2, parkedDeletes(98, limit)); err != nil {
				t.Fatalf("the same delete set again was refused: %v", err)
			}
			if got := parked(); got != limit {
				t.Fatalf("after the same delete set again %d delete ranges are parked, want %d", got, limit)
			}
			before := EncodeStateAsUpdateV1(doc, nil)
			if err := applyIn(t, doc, format.v2, parkedDeletes(98, limit+1)); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("one range past the cap was applied with err %v, want ErrInvalidUpdate", err)
			}
			if got := parked(); got != limit {
				t.Fatalf("after the refusal %d delete ranges are parked, want %d", got, limit)
			}
			if after := EncodeStateAsUpdateV1(doc, nil); string(after) != string(before) {
				t.Fatal("the refused update changed the document's state")
			}
			// Clocks 1, 3, ..., 2*limit-3 fill every gap between the parked
			// ranges, which join into one.
			gaps := make([]DeleteRange, 0, limit-1)
			for i := range limit - 1 {
				gaps = append(gaps, DeleteRange{Clock: uint64(2*i + 1), Len: 1})
			}
			if err := applyIn(t, doc, format.v2, deleteSetUpdate(map[ClientID][]DeleteRange{98: gaps})); err != nil {
				t.Fatalf("ranges that join the parked ones were refused: %v", err)
			}
			if got := parked(); got != 1 {
				t.Fatalf("after filling the gaps %d delete ranges are parked, want 1", got)
			}
		})
	}
}

// A parked delete range applies once the structs it names arrive, also after an
// apply that brings nothing for its client, and the document converges with a
// peer that saw every update in order.
func TestAParkedDeleteRangeAppliesOnceItsStructsArrive(t *testing.T) {
	writer := New(WithClientID(98))
	written := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { written.Insert(txn, 0, "abc", nil) })
	first := EncodeStateAsUpdateV1(writer, nil)
	sv := writer.StateVector()
	writer.Transact(func(txn *Transaction) { written.Insert(txn, 3, "def", nil) })
	second := EncodeStateAsUpdateV1(writer, sv)
	sv = writer.StateVector()
	writer.Transact(func(txn *Transaction) {
		written.Delete(txn, 4, 1)
		written.Delete(txn, 1, 1)
	})
	deletes := EncodeStateAsUpdateV1(writer, sv)
	other := New(WithClientID(11))
	otherText := other.GetText("u")
	other.Transact(func(txn *Transaction) { otherText.Insert(txn, 0, "x", nil) })
	unrelated := EncodeStateAsUpdateV1(other, nil)

	for _, format := range applyFormats {
		t.Run(format.name, func(t *testing.T) {
			doc := New(WithClientID(1))
			text := doc.GetText("t")
			for _, step := range []struct {
				what   string
				update []byte
				parked int
				text   string
			}{
				{"the deletes, before their structs", deletes, 2, ""},
				{"an update with nothing of client 98", unrelated, 2, ""},
				{"the first half of the structs", first, 1, "ac"},
				{"the rest of the structs", second, 0, "acdf"},
			} {
				if err := applyIn(t, doc, format.v2, step.update); err != nil {
					t.Fatalf("%s: %v", step.what, err)
				}
				if got := doc.PendingStats().DeleteRanges; got != step.parked {
					t.Fatalf("after %s %d delete ranges are parked, want %d", step.what, got, step.parked)
				}
				if got := text.ToString(); got != step.text {
					t.Fatalf("after %s the text is %q, want %q", step.what, got, step.text)
				}
			}
			inOrder := New(WithClientID(2))
			for _, update := range [][]byte{first, second, deletes, unrelated} {
				if err := ApplyUpdateV1(inOrder, update, nil); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := text.ToString(), inOrder.GetText("t").ToString(); got != want {
				t.Fatalf("the text is %q, the in-order peer's %q", got, want)
			}
		})
	}
}

// A stored state holding more parked delete ranges than the cap loads into a
// document whose cap is set once the state is in (SetMaxPendingItems); a
// document capped first refuses it. After the load, an update that adds a
// range is refused and one that adds none applies.
func TestAStoredStateOverTheCapLoadsWhenTheCapIsSetAfterIt(t *testing.T) {
	const limit = 10
	stored := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
	if err := ApplyUpdateV1(stored, parkedDeletes(98, limit+5), nil); err != nil {
		t.Fatal(err)
	}
	state := EncodeStateAsUpdateV1(stored, nil)
	if err := ApplyUpdateV1(New(WithMaxPendingItems(limit)), state, nil); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("a document capped at %d loaded the state with err %v, want ErrInvalidUpdate", limit, err)
	}
	doc := New(WithClientID(2), WithMaxPendingItems(math.MaxInt))
	if err := ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatal(err)
	}
	doc.SetMaxPendingItems(limit)
	if got := doc.PendingStats().DeleteRanges; got != limit+5 {
		t.Fatalf("the loaded document parks %d delete ranges, want %d", got, limit+5)
	}
	if err := ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatalf("the state sent again, which adds no range, was refused: %v", err)
	}
	if err := ApplyUpdateV1(doc, parkedDeletes(97, 1), nil); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("an update adding a range past the cap was applied with err %v, want ErrInvalidUpdate", err)
	}
}
