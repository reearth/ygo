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

// A keystroke must not pay for parked delete ranges of clients it brings
// nothing for, whether they belong to one client or to many.
func TestUnit_ApplyUpdate_AKeystrokeCostsNothingForParkedDeleteRanges(t *testing.T) {
	const n, keystrokes = 100_000, 100
	state := textState(7, strings.Repeat("a", 8))
	typed := typedKeystrokes(t, state, keystrokes)
	manyClients := make(map[ClientID][]DeleteRange, n)
	for i := range n {
		manyClients[ClientID(1000+i)] = []DeleteRange{{Clock: 0, Len: 1}}
	}
	shapes := []struct {
		name   string
		parked []byte
	}{
		{"nothing parked", nil},
		{"one client", parkedDeletes(98, n)},
		{"many clients", deleteSetUpdate(manyClients)},
	}
	var allocated [3]uint64
	var took [3]time.Duration
	for i, shape := range shapes {
		doc := New()
		if err := ApplyUpdateV1(doc, state, nil); err != nil {
			t.Fatal(err)
		}
		want := 0
		if shape.parked != nil {
			want = n
			if err := ApplyUpdateV1(doc, shape.parked, nil); err != nil {
				t.Fatal(err)
			}
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		for _, keystroke := range typed {
			if err := ApplyUpdateV1(doc, keystroke, nil); err != nil {
				t.Fatal(err)
			}
		}
		took[i] = time.Since(start) / keystrokes
		runtime.ReadMemStats(&after)
		allocated[i] = (after.TotalAlloc - before.TotalAlloc) / keystrokes
		if got := doc.PendingStats().DeleteRanges; got != want {
			t.Fatalf("%s: after the keystrokes %d delete ranges are parked, want %d", shape.name, got, want)
		}
		t.Logf("%s: a keystroke allocated %d bytes in %s", shape.name, allocated[i], took[i])
	}
	for i := 1; i < len(shapes); i++ {
		if allocated[i] > allocated[0]+1<<10 {
			t.Errorf("%s: a keystroke allocated %d bytes, %d with nothing parked", shapes[i].name, allocated[i], allocated[0])
		}
		if limit := 10*took[0] + 200*time.Microsecond; took[i] > limit {
			t.Errorf("%s: a keystroke took %s, %s with nothing parked, want at most %s", shapes[i].name, took[i], took[0], limit)
		}
	}
}

// typedKeystrokes are the updates of n characters a peer of state types into
// its text, one at a time.
func typedKeystrokes(t *testing.T, state []byte, n int) [][]byte {
	t.Helper()
	peer := New(WithClientID(11))
	if err := ApplyUpdateV1(peer, state, nil); err != nil {
		t.Fatal(err)
	}
	txt := peer.GetText("t")
	updates := make([][]byte, n)
	for i := range n {
		sv := peer.StateVector()
		peer.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "k", nil) })
		updates[i] = EncodeStateAsUpdateV1(peer, sv)
	}
	return updates
}

// Parked delete ranges are capped as parked items are (WithMaxPendingItems):
// an update whose delete set would grow the parked ranges past the cap is
// refused with ErrInvalidUpdate before it changes anything. An update that
// adds no range is never refused.
func TestUnit_ApplyUpdate_ParkedDeleteRangesAreCapped(t *testing.T) {
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

// Parked ranges that an update's own structs resolve do not count against the
// cap the update's new ranges are checked by.
func TestUnit_ApplyUpdate_ParkedRangesTheUpdateResolvesMakeRoom(t *testing.T) {
	const limit = 10
	// Client 98's structs at clocks 0-3 resolve the parked ranges at 0 and 2.
	structs := textState(98, "abcd")
	missing := func(n int) []byte {
		ranges := make([]DeleteRange, n)
		for i := range n {
			ranges[i] = DeleteRange{Clock: uint64(2 * i), Len: 1}
		}
		update, err := MergeUpdatesV1(structs, deleteRangesUpdate(99, ranges...))
		if err != nil {
			t.Fatal(err)
		}
		return update
	}
	for _, format := range applyFormats {
		t.Run(format.name, func(t *testing.T) {
			doc := New(WithMaxPendingItems(limit))
			if err := applyIn(t, doc, format.v2, parkedDeletes(98, limit)); err != nil {
				t.Fatal(err)
			}
			before := EncodeStateAsUpdateV1(doc, nil)
			updates := observed(doc)
			if err := applyIn(t, doc, format.v2, missing(3)); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("an update leaving %d ranges parked was applied with err %v, want ErrInvalidUpdate", limit+1, err)
			}
			requireUnchanged(t, doc, before, "", updates)
			if err := applyIn(t, doc, format.v2, missing(2)); err != nil {
				t.Fatalf("an update leaving %d ranges parked was refused: %v", limit, err)
			}
			if got := doc.PendingStats().DeleteRanges; got != limit {
				t.Fatalf("%d delete ranges are parked, want %d", got, limit)
			}
			if got := doc.GetText("t").ToString(); got != "bd" {
				t.Fatalf("the text is %q, want %q", got, "bd")
			}
		})
	}
}

// A parked delete range applies once the structs it names arrive, also after an
// apply that brings nothing for its client, and the document converges with a
// peer that saw every update in order.
func TestUnit_ApplyUpdate_AParkedDeleteRangeAppliesOnceItsStructsArrive(t *testing.T) {
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
func TestUnit_SetMaxPendingItems_AStoredStateOverTheCapLoadsWhenTheCapIsSetAfterIt(t *testing.T) {
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

// A document holding more parked items than its cap must still apply an
// update whose items wait only on each other within it.
func TestUnit_SetMaxPendingItems_ADocumentOverTheItemCapAppliesAnUpdateThatResolvesWithin(t *testing.T) {
	const limit, parked = 10, 19
	gapped := New(WithClientID(50))
	gappedText := gapped.GetText("t")
	gapped.Transact(func(txn *Transaction) { gappedText.Insert(txn, 0, "a", nil) })
	sv := gapped.StateVector()
	for range parked {
		gapped.Transact(func(txn *Transaction) { gappedText.Insert(txn, 0, "b", nil) })
	}
	afterGap := EncodeStateAsUpdateV1(gapped, sv)

	// Client 70's item has client 61's as its right origin, and client 70's
	// group comes first in the update.
	first := New(WithClientID(61))
	firstText := first.GetText("u")
	first.Transact(func(txn *Transaction) { firstText.Insert(txn, 0, "x", nil) })
	second := New(WithClientID(70))
	if err := ApplyUpdateV1(second, EncodeStateAsUpdateV1(first, nil), nil); err != nil {
		t.Fatal(err)
	}
	secondText := second.GetText("u")
	second.Transact(func(txn *Transaction) { secondText.Insert(txn, 0, "y", nil) })
	within := EncodeStateAsUpdateV1(second, nil)

	for _, format := range applyFormats {
		t.Run(format.name, func(t *testing.T) {
			doc := New(WithMaxPendingItems(math.MaxInt))
			if err := applyIn(t, doc, format.v2, afterGap); err != nil {
				t.Fatal(err)
			}
			doc.SetMaxPendingItems(limit)
			if got := doc.PendingStats().Items; got != parked {
				t.Fatalf("%d items are parked, want %d", got, parked)
			}
			if err := applyIn(t, doc, format.v2, within); err != nil {
				t.Fatalf("an update whose items resolve within it was refused: %v", err)
			}
			if got := doc.GetText("u").ToString(); got != "yx" {
				t.Fatalf("the text is %q, want %q", got, "yx")
			}
		})
	}
}

// deleteRangesUpdate is a V1 update of no structs whose delete set names the
// ranges of one client.
func deleteRangesUpdate(client ClientID, ranges ...DeleteRange) []byte {
	return deleteSetUpdate(map[ClientID][]DeleteRange{client: ranges})
}

// observed counts the updates doc's OnUpdate observers are handed.
func observed(doc *Doc) *int {
	n := new(int)
	doc.OnUpdate(func([]byte, any) { *n++ })
	return n
}

// requireUnchanged fails unless doc's state is before, its text "t" is text
// and its OnUpdate observers were handed no update.
func requireUnchanged(t *testing.T, doc *Doc, before []byte, text string, updates *int) {
	t.Helper()
	if after := EncodeStateAsUpdateV1(doc, nil); string(after) != string(before) {
		t.Fatalf("the document's state changed: %d bytes before, %d after", len(before), len(after))
	}
	if got := doc.GetText("t").ToString(); got != text {
		t.Fatalf("the text changed from %q to %q", text, got)
	}
	if *updates != 0 {
		t.Fatalf("the observers were handed %d updates, want none", *updates)
	}
}

// A refused over-cap update must leave the document's state, text, parked
// deletes and observers unchanged.
func TestUnit_ApplyUpdate_RefusedParkedDeletesLeaveTheDocumentUnchanged(t *testing.T) {
	const limit = 10
	writer := New(WithClientID(5))
	written := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { written.Insert(txn, 0, "hello", nil) })
	state := EncodeStateAsUpdateV1(writer, nil)
	sv := writer.StateVector()
	writer.Transact(func(txn *Transaction) {
		written.Insert(txn, 5, "!", nil)
		written.Delete(txn, 1, 2)
	})
	edit := EncodeStateAsUpdateV1(writer, sv)
	over := make([]DeleteRange, 0, limit+1)
	for i := range limit + 1 {
		over = append(over, DeleteRange{Clock: uint64(2 * i), Len: 1})
	}
	refused, err := MergeUpdatesV1(edit, deleteRangesUpdate(98, over...))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range applyFormats {
		t.Run(format.name, func(t *testing.T) {
			doc := New(WithClientID(1))
			if err := ApplyUpdateV1(doc, state, nil); err != nil {
				t.Fatal(err)
			}
			doc.SetMaxPendingItems(limit)
			text := doc.GetText("t")
			before, textBefore := EncodeStateAsUpdateV1(doc, nil), text.ToString()
			updates := observed(doc)
			if err := applyIn(t, doc, format.v2, refused); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("the update was applied with err %v, want ErrInvalidUpdate", err)
			}
			requireUnchanged(t, doc, before, textBefore, updates)
			if got := doc.PendingStats().DeleteRanges; got != 0 {
				t.Fatalf("after the refusal %d delete ranges are parked, want none", got)
			}
			if err := applyIn(t, doc, format.v2, edit); err != nil {
				t.Fatalf("the edit alone was refused: %v", err)
			}
			if got := text.ToString(); got != "hlo!" {
				t.Fatalf("after the edit alone the text is %q, want %q", got, "hlo!")
			}
			if *updates != 1 {
				t.Fatalf("the observers were handed %d updates for the edit alone, want 1", *updates)
			}
		})
	}
}

// decodeDS is the delete set of a V1 update.
func decodeDS(t *testing.T, update []byte) DeleteSet {
	t.Helper()
	ds, err := deleteSetOf(newPendingScanner(update, false), false)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// An update that carries more delete ranges than the cap, each deleting a
// struct the update itself brings, parks none of them and applies: the check
// reads the clocks the update's structs leave, not the store's as the apply
// begins. The structs of one client sit behind those of another in the
// update (they are inserted between them), so some integrate only once a
// later client group is in.
func TestUnit_ApplyUpdate_DeletingMoreThanTheCapOfItsOwnStructsApplies(t *testing.T) {
	const limit = 10
	first, second := New(WithClientID(1)), New(WithClientID(2))
	firstText, secondText := first.GetText("t"), second.GetText("t")
	first.Transact(func(txn *Transaction) { firstText.Insert(txn, 0, strings.Repeat("ab", 2*limit), nil) })
	if err := ApplyUpdateV1(second, EncodeStateAsUpdateV1(first, nil), nil); err != nil {
		t.Fatal(err)
	}
	for i := range 2 * limit {
		second.Transact(func(txn *Transaction) { secondText.Insert(txn, 3*i+1, "c", nil) })
	}
	for i := range 2 * limit {
		second.Transact(func(txn *Transaction) { secondText.Delete(txn, 2*i, 1) })
	}
	want := secondText.ToString()
	state := EncodeStateAsUpdateV1(second, nil)
	if got := len(decodeDS(t, state).clients[1]); got <= limit {
		t.Fatalf("the update deletes %d ranges of client 1, want more than %d", got, limit)
	}
	for _, format := range applyFormats {
		t.Run(format.name, func(t *testing.T) {
			doc := New()
			doc.SetMaxPendingItems(limit)
			if err := applyIn(t, doc, format.v2, state); err != nil {
				t.Fatalf("the update was refused: %v", err)
			}
			if got := doc.GetText("t").ToString(); got != want {
				t.Fatalf("the text is %q, want %q", got, want)
			}
			if got := doc.PendingStats().DeleteRanges; got != 0 {
				t.Fatalf("%d delete ranges are parked, want none", got)
			}
		})
	}
}

// A malformed update of several groups must be refused with the error text
// it has on main, so the parked-delete check adds no layer of wrapping.
func TestUnit_ApplyUpdate_AMalformedMultiGroupUpdateKeepsItsErrorText(t *testing.T) {
	want := wrapUpdateErr(ErrInvalidUpdate).Error()
	merged, err := MergeUpdatesV1(textState(5, "abc"), textState(6, "def"))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range applyFormats {
		t.Run(format.name, func(t *testing.T) {
			update := merged
			if format.v2 {
				if update, err = UpdateV1ToV2(merged); err != nil {
					t.Fatal(err)
				}
			}
			truncated := update[:len(update)-4]
			var got error
			if format.v2 {
				got = ApplyUpdateV2(New(), truncated, nil)
			} else {
				got = ApplyUpdateV1(New(), truncated, nil)
			}
			if !errors.Is(got, ErrInvalidUpdate) || got.Error() != want {
				t.Fatalf("the truncated update was applied with err %v, want %q", got, want)
			}
		})
	}
}
