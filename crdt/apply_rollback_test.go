package crdt

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// wholeOrNothing is boundTo on a document with whole-or-nothing applies.
func wholeOrNothing(t *testing.T, state []byte, bound int) *Doc {
	t.Helper()
	return boundTo(t, state, bound, WithWholeOrNothingApplies(true))
}

// observers counts the callbacks doc's observers are handed: its updates,
// transactions and subdocuments, and the events of its text and map.
func observers(doc *Doc) *int {
	n := new(int)
	doc.OnUpdate(func([]byte, any) { *n++ })
	doc.OnAfterTransaction(func(*Transaction) { *n++ })
	doc.OnSubdocs(func(SubdocsEvent) { *n++ })
	doc.GetText("t").Observe(func(YTextEvent) { *n++ })
	doc.GetMap("m").Observe(func(YMapEvent) { *n++ })
	return n
}

// parkedState encodes what doc holds parked: its parked items and pending
// deletes, and nothing it integrated.
func parkedState(doc *Doc) []byte { return EncodeStateAsUpdateV1(doc, doc.StateVector()) }

// listAndMap is chainedList(l) with a map whose key "k" client 5 set.
func listAndMap(t *testing.T, l int) []byte {
	t.Helper()
	doc := chainedList(t, l)
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", "kept") })
	return EncodeStateAsUpdateV1(doc, nil)
}

// editsThenScans is what client 9 sends a document holding state: edits, which
// type into the text, set "k" over client 5's value and set a new key, and then
// in the same update n items that each scan the text (originAtTheStart).
func editsThenScans(t *testing.T, state []byte, n int) (edits, update []byte) {
	t.Helper()
	peer := New(WithClientID(9))
	if err := ApplyUpdateV1(peer, state, nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	text, m := peer.GetText("t"), peer.GetMap("m")
	peer.Transact(func(txn *Transaction) {
		text.Insert(txn, 3, "typed", nil)
		m.Set(txn, "k", "refused")
		m.Set(txn, "new", 1)
	})
	edits = EncodeStateAsUpdateV1(peer, sv)
	update, err := MergeUpdatesV1(edits, originAtTheStartFrom(peer.StateVector().Clock(9), n))
	if err != nil {
		t.Fatal(err)
	}
	return edits, update
}

// A remote apply that fails on a document with whole-or-nothing applies, past
// the bound or for bytes that do not decode after structs that did, leaves the
// document byte for byte as it was, a map key it replaced included, and hands
// its observers nothing, every time it is sent, and the document then goes on
// like one that never saw it. Without the option the same apply keeps the
// edits.
func TestAWholeOrNothingApplyThatFailsLeavesTheDocumentAsItWas(t *testing.T) {
	const l, n, bound = 200, 20, 1_000
	state := listAndMap(t, l)
	edits, update := editsThenScans(t, state, n)
	malformed := append(edits[:len(edits)-1:len(edits)-1], 0xff, 0xff, 0xff)

	partway := boundTo(t, state, bound)
	if err := ApplyUpdateV1(partway, update, fromAClient); !errors.Is(err, ErrIntegrateScanBudget) {
		t.Fatalf("without the option the apply returned %v, want ErrIntegrateScanBudget", err)
	}
	if got, _ := partway.GetMap("m").Get("k"); got != "refused" {
		t.Fatalf("without the option the apply left k = %v, want the edits kept", got)
	}

	for _, refusal := range []struct {
		name   string
		update []byte
		apply  func(doc *Doc, update []byte, origin any) error
		want   error
	}{
		{"V1 past the bound", update, applyForms[0].apply, ErrIntegrateScanBudget},
		{"V2 past the bound", update, applyForms[1].apply, ErrIntegrateScanBudget},
		{"a delete set that does not decode", malformed, applyForms[0].apply, ErrInvalidUpdate},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			doc := wholeOrNothing(t, state, bound)
			before := EncodeStateAsUpdateV1(doc, nil)
			fired := observers(doc)
			for send := range 10 {
				if err := refusal.apply(doc, refusal.update, fromAClient); !errors.Is(err, refusal.want) {
					t.Fatalf("send %d returned %v, want %v", send, err, refusal.want)
				}
				if !bytes.Equal(EncodeStateAsUpdateV1(doc, nil), before) {
					t.Fatalf("send %d changed the document", send)
				}
			}
			if *fired != 0 {
				t.Fatalf("the observers were handed %d callbacks, want none", *fired)
			}
			requireText(t, doc, l, 0)
			m := doc.GetMap("m")
			if got, _ := m.Get("k"); got != "kept" {
				t.Fatalf("k = %v, want kept", got)
			}
			if _, ok := m.Get("new"); ok {
				t.Fatal("the refused key new is set")
			}
			requireStoreWhole(t, doc, &doc.GetText("t").abstractType, &m.abstractType)

			twin := boundTo(t, state, 0)
			for _, d := range []*Doc{doc, twin} {
				if err := ApplyUpdateV1(d, edits, nil); err != nil {
					t.Fatal(err)
				}
			}
			requireSameState(t, doc, twin)
		})
	}

	doc := wholeOrNothing(t, state, bound)
	if landed, err := ApplyUpdateV1Landed(doc, update, fromAClient); landed != nil || !errors.Is(err, ErrIntegrateScanBudget) {
		t.Fatalf("ApplyUpdateV1Landed returned %d bytes and %v, want none and ErrIntegrateScanBudget", len(landed), err)
	}
}

// typedInto is the update of one character a peer holding state types into its
// text.
func typedInto(t *testing.T, state []byte) []byte {
	t.Helper()
	peer := New(WithClientID(11))
	if err := ApplyUpdateV1(peer, state, nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	text := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { text.Insert(txn, 3, "k", nil) })
	return EncodeStateAsUpdateV1(peer, sv)
}

// A rollback gives a text the formatting flags it had, though the refused apply
// integrated format markers into it first (Item.integrate sets them). A text
// that held no marker so skips the format cleanup after the refusal: a Delete
// does not walk it for dangling markers, and a remote change to it commits no
// cleanup transaction.
func TestAWholeOrNothingRollbackRestoresTheTextsFormattingFlags(t *testing.T) {
	const l, n, bound = 200, 20, 100
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	peer := New(WithClientID(9))
	if err := ApplyUpdateV1(peer, state, nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	pt := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { pt.Format(txn, 0, 3, Attributes{"bold": true}) })
	update, err := MergeUpdatesV1(EncodeStateAsUpdateV1(peer, sv), originAtTheStartFrom(peer.StateVector().Clock(9), n))
	if err != nil {
		t.Fatal(err)
	}
	keystroke := typedInto(t, state)

	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			doc := wholeOrNothing(t, state, bound)
			text := doc.GetText("t")
			flags := func() formatFlags {
				return formatFlags{has: text.hasFormatting, clean: text.cleanFormatting, gen: text.fmtGen}
			}
			before := flags()
			if err := form.apply(doc, update, fromAClient); !errors.Is(err, ErrIntegrateScanBudget) {
				t.Fatalf("the apply returned %v, want ErrIntegrateScanBudget", err)
			}
			if got := flags(); got != before {
				t.Fatalf("the text's formatting flags are %+v after the refusal, want %+v", got, before)
			}

			// The cleanup walk starts at the first live item, which would move
			// the text's cache of it past the head the delete removed.
			head := text.start
			doc.Transact(func(txn *Transaction) { text.Delete(txn, 0, 1) })
			if text.firstLiveCache != head {
				t.Fatal("the delete walked the text for dangling format markers")
			}

			commits := 0
			doc.OnAfterTransaction(func(*Transaction) { commits++ })
			if err := ApplyUpdateV1(doc, keystroke, nil); err != nil {
				t.Fatal(err)
			}
			if commits != 1 {
				t.Fatalf("a remote keystroke committed %d transactions, want 1 and no format cleanup", commits)
			}
		})
	}
}

// A document with whole-or-nothing applies refuses a remote update that
// carries a move or a subdocument, and keeps nothing of it. With the option off
// both apply.
func TestAWholeOrNothingDocumentRefusesRemoteMovesAndSubdocuments(t *testing.T) {
	writer := New(WithClientID(3))
	array, m := writer.GetArray("a"), writer.GetMap("m")
	writer.Transact(func(txn *Transaction) { array.Push(txn, []any{1, 2, 3}) })
	pushed := EncodeStateAsUpdateV1(writer, nil)
	sv := writer.StateVector()
	writer.Transact(func(txn *Transaction) { array.Move(txn, 0, 3) })
	moved := EncodeStateAsUpdateV1(writer, sv)
	sv = writer.StateVector()
	sub := New()
	writer.Transact(func(txn *Transaction) { m.Set(txn, "sub", sub) })
	withSub := EncodeStateAsUpdateV1(writer, sv)

	for _, form := range applyForms {
		for _, on := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/on=%t", form.name, on), func(t *testing.T) {
				doc := New(WithWholeOrNothingApplies(on))
				var added []*Doc
				doc.OnSubdocs(func(e SubdocsEvent) { added = append(added, e.Added...) })
				if err := form.apply(doc, pushed, nil); err != nil {
					t.Fatal(err)
				}
				for _, update := range [][]byte{moved, withSub} {
					before := EncodeStateAsUpdateV1(doc, nil)
					fired := observers(doc)
					err := form.apply(doc, update, nil)
					if !on {
						if err != nil {
							t.Fatal(err)
						}
						continue
					}
					if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), "takes no remote move or subdocument") {
						t.Fatalf("the apply returned %v, want the refusal", err)
					}
					if !bytes.Equal(EncodeStateAsUpdateV1(doc, nil), before) || *fired != 0 {
						t.Fatalf("the refusal changed the document or handed its observers %d callbacks", *fired)
					}
				}
				wantArray, wantAdded := fmt.Sprint(array.ToSlice()), 1
				if on {
					wantArray, wantAdded = "[1 2 3]", 0
				}
				if got := fmt.Sprint(doc.GetArray("a").ToSlice()); got != wantArray {
					t.Fatalf("the array is %s, want %s", got, wantArray)
				}
				if len(added) != wantAdded {
					t.Fatalf("%d subdocuments were added, want %d", len(added), wantAdded)
				}
			})
		}
	}
}

// When the drain of the parked queue trips what the apply left of the bound,
// the drain is rolled back whole: the queue is parked again byte for byte as
// the drain found it, the apply that filled the gap stands and reports only its
// own item, and each send of the parked items, which trip the bound themselves,
// is refused. A later apply from another sender whose drain has more of the
// bound left than that drain began with tries the queue once, and the applies
// after it spend nothing on it; an apply with no bound, as from another node,
// completes it.
func TestAWholeOrNothingDrainPastTheBoundIsRolledBackAndTheApplyStands(t *testing.T) {
	const l, n, bound = 200, 20, 1_000
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	gap, parked := originAtTheStartFrom(0, 1), originAtTheStartFrom(1, n)
	edits := [][]byte{editFrom(11), editFrom(12), editFrom(13), editFrom(14)}
	want := boundTo(t, state, 0)
	for _, update := range append([][]byte{parked, gap}, edits...) {
		if err := ApplyUpdateV1(want, update, nil); err != nil {
			t.Fatal(err)
		}
	}

	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			doc := wholeOrNothing(t, state, bound)
			if err := form.apply(doc, parked, fromAClient); err != nil {
				t.Fatal(err)
			}
			queue, stats := parkedState(doc), fmt.Sprintf("%+v", doc.PendingStats())
			var updates [][]byte
			doc.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
			// The gap's item scans 199 items, which leaves the drain 801: four
			// of the parked items, which scan 200 each.
			if err := form.apply(doc, gap, fromAClient); err != nil {
				t.Fatalf("the apply that filled the gap returned %v", err)
			}
			requireText(t, doc, l, 1)
			requireStoreWhole(t, doc, &doc.GetText("t").abstractType)
			if !bytes.Equal(parkedState(doc), queue) {
				t.Fatal("the parked queue differs from the one the drain found")
			}
			if got := fmt.Sprintf("%+v", doc.PendingStats()); got != stats {
				t.Fatalf("the pending queue is %s, want %s", got, stats)
			}
			if len(updates) != 1 {
				t.Fatalf("the observers were handed %d updates, want 1", len(updates))
			}
			relayed := boundTo(t, state, 0)
			if err := ApplyUpdateV1(relayed, updates[0], nil); err != nil {
				t.Fatal(err)
			}
			requireText(t, relayed, l, 1)

			before := EncodeStateAsUpdateV1(doc, nil)
			for send := range 3 {
				if err := form.apply(doc, parked, fromAClient); !errors.Is(err, ErrIntegrateScanBudget) {
					t.Fatalf("send %d of the parked items returned %v, want ErrIntegrateScanBudget", send, err)
				}
				if !bytes.Equal(EncodeStateAsUpdateV1(doc, nil), before) {
					t.Fatalf("send %d of the parked items changed the document", send)
				}
			}
			spent := -1
			doc.OnAfterTransaction(func(txn *Transaction) { spent = bound - int(txn.scanLeft) })
			for i, edit := range edits[:3] {
				if err := form.apply(doc, edit, fromAClient); err != nil {
					t.Fatal(err)
				}
				want := 0
				if i == 0 {
					want = bound // the whole bound, more than the 801 the drain began with
				}
				if spent != want || !bytes.Equal(parkedState(doc), queue) {
					t.Fatalf("edit %d from another client spent %d of the bound, want %d, and must leave the queue as it was", i, spent, want)
				}
			}
			if err := form.apply(doc, edits[3], nil); err != nil {
				t.Fatal(err)
			}
			if doc.store.pending != nil {
				t.Fatalf("%d items parked after the update completed", len(doc.store.pending.items))
			}
			requireSameState(t, doc, want)
		})
	}
}

// itemAfter is one string item of client at clock, with origin as its left
// origin and no right origin.
func itemAfter(client ClientID, clock uint64, origin ID, s string) []byte {
	enc := encoding.NewEncoder()
	for _, v := range []uint64{1, 1, uint64(client), clock} { // one client, one struct
		enc.WriteVarUint(v)
	}
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(uint64(origin.Client))
	enc.WriteVarUint(origin.Clock)
	enc.WriteVarString(s)
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

// A drain rolled back gives back what it changed of a parked item: client 99's
// "abc", which the drain integrated and the next parked item's origin then
// split, is parked again whole, where without the option the drain keeps "abc".
// Once client 99's item arrives again, relayed, both parked items apply.
func TestAWholeOrNothingDrainGivesBackAParkedItemItSplit(t *testing.T) {
	const l = 200
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	start := ID{Client: 5, Clock: 0}
	abc := itemAfter(99, 1, start, "abc")                // behind client 99's clock 0
	r := itemAfter(98, 0, ID{Client: 99, Clock: 2}, "r") // the middle of "abc"
	fill := itemAfter(99, 0, start, "f")                 // client 99's clock 0
	want := boundTo(t, state, 0)
	for _, update := range [][]byte{abc, r, fill} {
		if err := ApplyUpdateV1(want, update, nil); err != nil {
			t.Fatal(err)
		}
	}
	// fill scans 199 items and "abc" 200, so the bound stops the drain in r's
	// scan, after r's origin split "abc" (tryIntegrate).
	const bound = 399

	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			keeps := boundTo(t, state, bound)
			for _, update := range [][]byte{abc, r, fill} {
				if err := form.apply(keeps, update, fromAClient); err != nil {
					t.Fatal(err)
				}
			}
			if got, parked := keeps.StateVector().Clock(99), len(keeps.store.pending.items); got != 4 || parked != 1 {
				t.Fatalf("without the option client 99's clock is %d and %d items are parked, want abc integrated and r parked", got, parked)
			}

			doc := wholeOrNothing(t, state, bound)
			for _, update := range [][]byte{abc, r} {
				if err := form.apply(doc, update, fromAClient); err != nil {
					t.Fatal(err)
				}
			}
			queue := parkedState(doc)
			if err := form.apply(doc, fill, fromAClient); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(parkedState(doc), queue) {
				t.Fatal("the parked queue differs from the one the drain found")
			}
			if got := len(doc.store.clients[99]); got != 1 {
				t.Fatalf("client 99 has %d structs, want f alone", got)
			}
			requireStoreWhole(t, doc, &doc.GetText("t").abstractType)

			if err := form.apply(doc, abc, nil); err != nil {
				t.Fatal(err)
			}
			if doc.store.pending != nil {
				t.Fatalf("%d items still parked", len(doc.store.pending.items))
			}
			requireSameState(t, doc, want)
		})
	}
}

// A parked struct whose integration panics for a reason other than the bound
// fails the drain of a document with whole-or-nothing applies, so the apply
// that filled the gap returns ErrInvalidUpdate naming the panic, and is rolled
// back whole, bounded or not: the document is as it was, and so is what it
// holds parked, the struct that panicked included, though the apply parked an
// item and delete ranges of its own before the drain. No observer fires.
func TestAWholeOrNothingDrainFailsOnAPanicThatIsNotTheBounds(t *testing.T) {
	const l, n = 200, 2
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	parked := originAtTheStartFrom(1, n)
	gap, err := MergeUpdatesV1(originAtTheStartFrom(0, 1), itemAfter(76, 0, ID{Client: 77, Clock: 0}, "z"), parkedDeletes(98, 3))
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range applyForms {
		for _, bound := range []int{0, l - 1, 1 << 20} {
			t.Run(fmt.Sprintf("%s/bound=%d", form.name, bound), func(t *testing.T) {
				doc := wholeOrNothing(t, state, bound)
				for _, update := range [][]byte{parked, parkedDeletes(97, 2)} {
					if err := form.apply(doc, update, fromAClient); err != nil {
						t.Fatal(err)
					}
				}
				before, stats := EncodeStateAsUpdateV1(doc, nil), fmt.Sprintf("%+v", doc.PendingStats())
				fired := observers(doc)
				first := doc.store.pending.items[0]
				content := first.Content
				first.Content = panicOnLen{content}
				err := form.apply(doc, gap, fromAClient)
				first.Content = content
				if err == nil || errors.Is(err, ErrIntegrateScanBudget) || !errors.Is(err, ErrInvalidUpdate) ||
					!strings.Contains(err.Error(), "a parked struct that panics") {
					t.Fatalf("the apply that filled the gap returned %v, want ErrInvalidUpdate naming the panic", err)
				}
				if !bytes.Equal(EncodeStateAsUpdateV1(doc, nil), before) || *fired != 0 {
					t.Fatalf("the apply changed the document or handed its observers %d callbacks", *fired)
				}
				if got := fmt.Sprintf("%+v", doc.PendingStats()); got != stats {
					t.Fatalf("the pending queue is %s, want %s", got, stats)
				}
			})
		}
	}
}

// A keystroke costs a document with whole-or-nothing applies about what it
// costs one without, however much earlier updates left parked: the journal
// keeps the parked items, the clocks they wait on and the pending deletes by
// reference.
func TestAWholeOrNothingApplyCostsNothingForWhatIsParked(t *testing.T) {
	const l, n = 100, 20_000
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	keystroke := typedInto(t, state)
	for _, parked := range []struct {
		name   string
		update []byte
	}{
		{"items", originAtTheStartFrom(1, n)},
		{"delete ranges", parkedDeletes(98, n)},
	} {
		t.Run(parked.name, func(t *testing.T) {
			var allocated [2]uint64
			for i, on := range []bool{false, true} {
				doc := New(WithWholeOrNothingApplies(on))
				for _, update := range [][]byte{state, parked.update} {
					if err := ApplyUpdateV1(doc, update, nil); err != nil {
						t.Fatal(err)
					}
				}
				if stats := doc.PendingStats(); stats.Items+stats.DeleteRanges != n {
					t.Fatalf("%d items and %d delete ranges are parked, want %d in all", stats.Items, stats.DeleteRanges, n)
				}
				var err error
				if allocated[i], _, err = applyMeasured(doc, keystroke); err != nil {
					t.Fatal(err)
				}
				t.Logf("on=%t: the keystroke allocated %d bytes", on, allocated[i])
			}
			if off, on := allocated[0], allocated[1]; on > off+64<<10 {
				t.Fatalf("with %d %s parked, the keystroke allocated %d bytes with the option and %d without, want within %d",
					n, parked.name, on, off, 64<<10)
			}
		})
	}
}

// A park that merges an update's structs into the queue changes parked
// structs in place, as Yjs merges its pending update: here it grows a parked
// GC struct by the one sent over it. When the apply then fails on a document
// with whole-or-nothing applies, the queue is as the apply found it, and the
// struct that fills the gap drains it as before.
func TestAWholeOrNothingRollbackGivesBackTheQueueAParkMergedInto(t *testing.T) {
	doc := New(WithClientID(100), WithWholeOrNothingApplies(true))
	if err := applyHexes(t, doc, false, "01010201000100"); err != nil { // GC 2@1+1, behind 2@0
		t.Fatal(err)
	}
	before, stats := parkedState(doc), fmt.Sprintf("%+v", doc.PendingStats())
	fired := observers(doc)
	// GC 2@1+2, which the park merges into the parked struct, then a delete set
	// cut off after the first client it names.
	if err := applyHexes(t, doc, false, "01010201000201050101"); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("the cut update returned %v, want ErrInvalidUpdate", err)
	}
	if !bytes.Equal(parkedState(doc), before) || *fired != 0 {
		t.Fatalf("the refused update changed what is parked or handed its observers %d callbacks", *fired)
	}
	if got := fmt.Sprintf("%+v", doc.PendingStats()); got != stats {
		t.Fatalf("the pending queue is %s, want %s", got, stats)
	}
	if items := doc.store.pending.heldItems(); len(items) != 1 || items[0].ID.Clock != 1 || items[0].Content.Len() != 1 {
		t.Fatalf("the queue holds %d structs, want GC 2@1+1 as it was parked", len(items))
	}
	if err := applyHexes(t, doc, false, "01010200000100"); err != nil { // GC 2@0+1
		t.Fatal(err)
	}
	if got := svText(doc.StateVector()); got != "2:2," || doc.store.pending != nil {
		t.Fatalf("the gap left state vector %s and %v parked, want 2:2, and nothing", got, doc.store.pending)
	}
}

// loadNotifies reports whether child.Load() tells its parent, which a
// subdocument still attached to the document does and a detached one cannot.
func loadNotifies(parent, child *Doc) bool {
	loaded := 0
	stop := parent.OnSubdocs(func(ev SubdocsEvent) {
		for _, sub := range ev.Loaded {
			if sub == child {
				loaded++
			}
		}
	})
	defer stop()
	child.Load()
	return loaded == 1
}

// A refused update on a document with whole-or-nothing applies that had
// deleted an item carrying a subdocument leaves the subdocument as it was:
// still under its key, and attached, so its Load tells the document. The
// update carries only a delete and a struct, so the document takes it, and
// fails in the drain it starts, on a parked struct that panics.
func TestAWholeOrNothingRollbackKeepsADeletedSubdocumentAttached(t *testing.T) {
	doc := New(WithClientID(7), WithWholeOrNothingApplies(true))
	child := New(WithGUID("child"), WithShouldLoad(false))
	holder := doc.GetMap("t")
	doc.Transact(func(txn *Transaction) { holder.Set(txn, "doc", child) })

	// A peer deletes the key; its update carries the delete and nothing else.
	peer := New(WithClientID(2))
	if err := ApplyUpdateV1(peer, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	sv, peerHolder := peer.StateVector(), peer.GetMap("t")
	peer.Transact(func(txn *Transaction) { peerHolder.Delete(txn, "doc") })
	deleteKey := EncodeStateAsUpdateV1(peer, sv)

	// A writer's second character parks behind its first.
	writer := New(WithClientID(50))
	text := writer.GetText("w")
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 0, "a", nil) })
	first := EncodeStateAsUpdateV1(writer, nil)
	at := writer.StateVector()
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 1, "b", nil) })
	if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(writer, at), nil); err != nil {
		t.Fatal(err)
	}
	gap, err := MergeUpdatesV1(first, deleteKey)
	if err != nil {
		t.Fatal(err)
	}

	parked := doc.store.pending.items[0]
	content := parked.Content
	parked.Content = panicOnLen{content}
	err = ApplyUpdateV1(doc, gap, nil)
	parked.Content = content
	if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), "a parked struct that panics") {
		t.Fatalf("the update returned %v, want ErrInvalidUpdate naming the panic", err)
	}
	if entry, held := holder.Get("doc"); !held || entry != child {
		t.Fatalf("the refused update left %v under the key the subdocument held", entry)
	}
	if !loadNotifies(doc, child) {
		t.Fatal("the subdocument's Load told the document nothing: the refused update left it detached")
	}
}
