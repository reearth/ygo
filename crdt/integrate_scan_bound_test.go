package crdt

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// fromAClient is the origin boundTo bounds. Every other origin, such as that
// of an update relayed from another node or loaded from storage, has no bound.
const fromAClient = "client"

// boundTo is a document holding state, loaded with no bound, whose remote
// applies with origin fromAClient may scan at most bound items, made with opts.
func boundTo(t *testing.T, state []byte, bound int, opts ...DocOption) *Doc {
	t.Helper()
	doc := New(append(opts, WithIntegrateScanBudget(func(origin any) int {
		if origin == fromAClient {
			return bound
		}
		return 0
	}))...)
	if err := ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatal(err)
	}
	return doc
}

// applyForms are the remote applies a bound covers.
var applyForms = []struct {
	name  string
	apply func(doc *Doc, update []byte, origin any) error
}{
	{"V1", ApplyUpdateV1},
	{"V2", func(doc *Doc, update []byte, origin any) error {
		v2, err := UpdateV1ToV2(update)
		if err != nil {
			return err
		}
		return ApplyUpdateV2(doc, v2, origin)
	}},
}

// requireText requires doc's text to be chainedList's l characters, then n of
// client 9's.
func requireText(t *testing.T, doc *Doc, l, n int) {
	t.Helper()
	if got, want := doc.GetText("t").ToString(), strings.Repeat("c", l)+strings.Repeat("x", n); got != want {
		t.Fatalf("the text is %d characters, %d of them client 9's; want %d, then %d", len(got), strings.Count(got, "x"), l, n)
	}
}

// requireSameState requires doc to encode as want does.
func requireSameState(t *testing.T, doc, want *Doc) {
	t.Helper()
	if !bytes.Equal(EncodeStateAsUpdateV1(doc, nil), EncodeStateAsUpdateV1(want, nil)) {
		t.Fatal("the document differs from one that applied the same updates with no bound")
	}
}

// An apply whose scans pass the bound aborts partway with
// ErrIntegrateScanBudget and keeps what it integrated, and each send of the
// same update carries on from there until it completes. Each of the update's
// 20 items scans the 200-item list, so a bound of 1,000 holds five of them.
func TestAnApplyPastTheBoundKeepsWhatItIntegrated(t *testing.T) {
	const l, n, bound = 200, 20, 1_000
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	update := originAtTheStartFrom(0, n)
	want := boundTo(t, state, 0)
	if err := ApplyUpdateV1(want, update, nil); err != nil {
		t.Fatal(err)
	}

	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			doc := boundTo(t, state, bound)
			for send := 1; send <= n/5; send++ {
				err := form.apply(doc, update, fromAClient)
				if send < n/5 && !errors.Is(err, ErrIntegrateScanBudget) {
					t.Fatalf("send %d returned %v, want ErrIntegrateScanBudget", send, err)
				}
				if send == n/5 && err != nil {
					t.Fatalf("send %d, the last, returned %v", send, err)
				}
				requireText(t, doc, l, 5*send)
				requireStoreWhole(t, doc, &doc.GetText("t").abstractType)
			}
			requireSameState(t, doc, want)
		})
	}

	// ApplyUpdateV1Landed reports exactly what each aborted send kept.
	t.Run("landed", func(t *testing.T) {
		doc := boundTo(t, state, bound)
		for send := 1; send < n/5; send++ {
			before := EncodeStateAsUpdateV1(doc, nil)
			landed, err := ApplyUpdateV1Landed(doc, update, fromAClient)
			if !errors.Is(err, ErrIntegrateScanBudget) {
				t.Fatalf("send %d returned %v, want ErrIntegrateScanBudget", send, err)
			}
			forwarded := boundTo(t, before, 0)
			if err := ApplyUpdateV1(forwarded, landed, nil); err != nil {
				t.Fatal(err)
			}
			requireSameState(t, forwarded, doc)
		}
	})
}

// listAfterARun is the state of a text of l characters like chainedList's,
// whose first two client 5 typed as one run, so an item naming the first as its
// left origin splits the run.
func listAfterARun(t *testing.T, l int) []byte {
	t.Helper()
	a, b := New(WithClientID(5)), New(WithClientID(6))
	at := a.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "cc", nil) })
	writers := [2]*Doc{a, b}
	for i := 0; at.Len() < l; i++ {
		from, to := writers[i%2], writers[(i+1)%2]
		if i > 0 {
			text := from.GetText("t")
			from.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "c", nil) })
		}
		if err := ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, to.StateVector()), nil); err != nil {
			t.Fatal(err)
		}
	}
	return EncodeStateAsUpdateV1(a, nil)
}

// An update whose first item alone scans past the bound never completes, and
// each send leaves the document byte for byte as it was, whole and readable,
// though the item's left origin split a run before its scan: the commit merges
// the halves back. Applied with no bound, as from another node, it completes.
func TestAnUpdateWhoseItemScansPastTheBoundNeverCompletes(t *testing.T) {
	const l, n, bound = 200, 20, 100
	state := listAfterARun(t, l)
	update := originAtTheStartFrom(0, n)
	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			doc := boundTo(t, state, bound)
			before := EncodeStateAsUpdateV1(doc, nil)
			for send := range 10 {
				if err := form.apply(doc, update, fromAClient); !errors.Is(err, ErrIntegrateScanBudget) {
					t.Fatalf("send %d returned %v, want ErrIntegrateScanBudget", send, err)
				}
				if !bytes.Equal(EncodeStateAsUpdateV1(doc, nil), before) {
					t.Fatalf("send %d changed the document", send)
				}
			}
			requireStoreWhole(t, doc, &doc.GetText("t").abstractType)
			requireText(t, doc, l, 0)
			if err := form.apply(doc, update, nil); err != nil {
				t.Fatal(err)
			}
			requireText(t, doc, l, n)
		})
	}
	doc := boundTo(t, state, bound)
	if landed, err := ApplyUpdateV1Landed(doc, update, fromAClient); landed != nil || !errors.Is(err, ErrIntegrateScanBudget) {
		t.Fatalf("ApplyUpdateV1Landed returned %d bytes and %v, want none and ErrIntegrateScanBudget", len(landed), err)
	}
}

// When the drain of the parked queue passes what the apply left of the bound,
// it keeps what it integrated and parks the rest, and the apply that filled the
// gap succeeds: its sender did not send the parked items. Nothing tells that
// sender to send them again, so a later apply, from any sender, tries them
// again on what it leaves of its own bound: here, edits from other clients
// that scan nothing each leave the drain the whole bound and integrate some,
// until none is parked.
func TestADrainPastTheBoundKeepsWhatItIntegratedAndParksTheRest(t *testing.T) {
	const l, n, bound = 200, 20, 1_000
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	gap, parked := originAtTheStartFrom(0, 1), originAtTheStartFrom(1, n)
	elsewhere := make([][]byte, n)
	for i := range elsewhere {
		elsewhere[i] = editFrom(ClientID(11 + i))
	}
	want := boundTo(t, state, 0)
	for _, update := range append([][]byte{parked, gap}, elsewhere...) {
		if err := ApplyUpdateV1(want, update, nil); err != nil {
			t.Fatal(err)
		}
	}

	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			doc := boundTo(t, state, bound)
			if err := form.apply(doc, parked, fromAClient); err != nil {
				t.Fatal(err)
			}
			if got := len(doc.store.pending.items); got != n {
				t.Fatalf("%d items parked, want %d", got, n)
			}
			// The gap's item scans 199 items, which leaves the drain 801: four
			// of the parked items, which scan 200 each.
			if err := form.apply(doc, gap, fromAClient); err != nil {
				t.Fatalf("the apply that filled the gap returned %v", err)
			}
			requireText(t, doc, l, 5)
			requireStoreWhole(t, doc, &doc.GetText("t").abstractType)
			if got := len(doc.store.pending.items); got != n-4 {
				t.Fatalf("%d items parked after the drain, want %d", got, n-4)
			}
			requireText(t, boundTo(t, EncodeStateAsUpdateV1(doc, nil), 0), l, n+1)

			parkedNow := n - 4
			for i, update := range elsewhere {
				if err := form.apply(doc, update, fromAClient); err != nil {
					t.Fatalf("edit %d from another client returned %v", i, err)
				}
				if doc.store.pending == nil {
					continue
				}
				if got := len(doc.store.pending.items); got >= parkedNow {
					t.Fatalf("edit %d from another client left %d items parked, want fewer than %d", i, got, parkedNow)
				}
				parkedNow = len(doc.store.pending.items)
			}
			if doc.store.pending != nil {
				t.Fatalf("%d items parked after %d edits from other clients", len(doc.store.pending.items), n)
			}
			requireText(t, doc, l, n+1)
			requireStoreWhole(t, doc, &doc.GetText("t").abstractType)
			requireSameState(t, doc, want)
		})
	}
}

// editFrom is an update from client that sets a key of its own in a map, and
// so scans nothing.
func editFrom(client ClientID) []byte {
	doc := New(WithClientID(client))
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) { m.Set(txn, fmt.Sprint(client), 1) })
	return EncodeStateAsUpdateV1(doc, nil)
}

// characterAfter is an update of one character from client 9 at clock, with
// origin as its left origin and no right origin.
func characterAfter(clock uint64, origin ID) []byte {
	enc := encoding.NewEncoder()
	for _, v := range []uint64{1, 1, 9, clock} { // one client, one struct, client 9, clock
		enc.WriteVarUint(v)
	}
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(uint64(origin.Client))
	enc.WriteVarUint(origin.Clock)
	enc.WriteVarString("x")
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

// A parked item whose scan alone passes the bound is tried again only by a
// drain with more of the bound left than the scan that tripped had, or with no
// bound. One later apply that leaves its drain the whole bound finds that the
// item does not fit, the applies after it spend nothing on it, and an apply
// with no bound integrates it.
func TestAParkedItemPastTheBoundCostsTheAppliesAfterItNothing(t *testing.T) {
	const l, bound = 200, 100
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	// The gap, client 9's clock 0, follows chainedList's character 150 and
	// scans the 49 after it, which leaves the drain 51. Clock 1 scans 199.
	gap, stuck := characterAfter(0, ID{Client: 5, Clock: 75}), originAtTheStartFrom(1, 1)
	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			doc := boundTo(t, state, bound)
			spent := -1
			doc.OnAfterTransaction(func(txn *Transaction) { spent = bound - int(txn.scanLeft) })
			for _, update := range [][]byte{stuck, gap} {
				if err := form.apply(doc, update, fromAClient); err != nil {
					t.Fatal(err)
				}
			}
			if parked := doc.PendingStats().Items; spent != bound || parked != 1 {
				t.Fatalf("the apply that filled the gap spent %d of the bound and left %d items parked, want %d and 1", spent, parked, bound)
			}
			for i := range 5 {
				if err := form.apply(doc, editFrom(ClientID(11+i)), fromAClient); err != nil {
					t.Fatal(err)
				}
				want := 0
				if i == 0 {
					want = bound // the first has the whole bound, more than the 51 the drain had
				}
				if parked := doc.PendingStats().Items; spent != want || parked != 1 {
					t.Fatalf("edit %d from another client spent %d of the bound and left %d items parked, want %d and 1", i, spent, parked, want)
				}
			}
			if err := form.apply(doc, editFrom(16), nil); err != nil {
				t.Fatal(err)
			}
			if parked := doc.PendingStats().Items; parked != 0 {
				t.Fatalf("%d items parked after an apply with no bound", parked)
			}
			if got := strings.Count(doc.GetText("t").ToString(), "x"); got != 2 {
				t.Fatalf("the text holds %d of client 9's characters, want 2", got)
			}
			requireStoreWhole(t, doc, &doc.GetText("t").abstractType)
		})
	}
}

// A parked item the bound stopped in the drain is placed where it belongs
// when a later drain integrates it: the right neighbour its scan resolved was
// half of a run the commit merged back, and the item resolves it again.
func TestAParkedItemTheBoundStoppedIsPlacedWhenTriedAgain(t *testing.T) {
	const l = 200
	list := chainedList(t, l)
	text := list.GetText("t")
	end := list.StateVector().Clock(5)
	list.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "rr", nil) })
	state := EncodeStateAsUpdateV1(list, nil)
	gap := originAtTheStartFrom(0, 1)
	// Client 9's clock 1, between the start and the run's second character.
	enc := encoding.NewEncoder()
	for _, v := range []uint64{1, 1, 9, 1} { // one client, one struct, client 9, clock 1
		enc.WriteVarUint(v)
	}
	enc.WriteUint8(wireString | flagHasOrigin | flagHasRightOrigin)
	for _, v := range []uint64{5, 0, 5, end + 1} {
		enc.WriteVarUint(v)
	}
	enc.WriteVarString("p")
	enc.WriteVarUint(0) // delete set: no clients
	between := append([]byte(nil), enc.Bytes()...)
	// Client 13's second keystroke parks until its first arrives, relayed.
	typist := New(WithClientID(13))
	if err := ApplyUpdateV1(typist, state, nil); err != nil {
		t.Fatal(err)
	}
	typed := typist.GetText("t")
	keystrokes := make([][]byte, 0, 2)
	for range 2 {
		sv := typist.StateVector()
		typist.Transact(func(txn *Transaction) { typed.Insert(txn, typed.Len(), "q", nil) })
		keystrokes = append(keystrokes, EncodeStateAsUpdateV1(typist, sv))
	}
	want := boundTo(t, state, 0)
	for _, update := range [][]byte{between, keystrokes[1], gap, keystrokes[0]} {
		if err := ApplyUpdateV1(want, update, nil); err != nil {
			t.Fatal(err)
		}
	}

	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			// The gap's item scans 200 items, which leaves the drain 100: the
			// parked item's scan of 200 stops halfway.
			doc := boundTo(t, state, 300)
			for _, update := range [][]byte{between, keystrokes[1], gap} {
				if err := form.apply(doc, update, fromAClient); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(doc.store.pending.items); got != 2 {
				t.Fatalf("%d items parked, want the item the bound stopped and the keystroke", got)
			}
			if err := form.apply(doc, keystrokes[0], nil); err != nil {
				t.Fatal(err)
			}
			if doc.store.pending != nil {
				t.Fatalf("%d items still parked", len(doc.store.pending.items))
			}
			if got, want := doc.GetText("t").ToString(), want.GetText("t").ToString(); got != want {
				t.Fatalf("the text ends %q, want %q", got[l:], want[l:])
			}
			requireStoreWhole(t, doc, &doc.GetText("t").abstractType)
			requireSameState(t, doc, want)
		})
	}
}

// panicOnLen is a parked struct whose Len panics, for a bug or a malformed
// struct the drain meets.
type panicOnLen struct{ Content }

func (panicOnLen) Len() uint64 { panic("a parked struct that panics") }

// The drain recovers the bound's panic and drops a parked struct whose
// integration panics for any other reason, so the apply that filled the gap
// returns ErrInvalidUpdate naming that panic, bounded or not, and the queue
// keeps what the drain did not integrate: the struct after the dropped one,
// which waits on its clock. Under a bound of 199 the gap's item leaves the
// drain nothing of it.
func TestADrainDropsAStructThatPanicsNotForTheBound(t *testing.T) {
	const l, n = 200, 2
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	gap, parked := originAtTheStartFrom(0, 1), originAtTheStartFrom(1, n)
	for _, form := range applyForms {
		for _, bound := range []int{0, l - 1, 1 << 20} {
			t.Run(fmt.Sprintf("%s/bound=%d", form.name, bound), func(t *testing.T) {
				doc := boundTo(t, state, bound)
				if err := form.apply(doc, parked, fromAClient); err != nil {
					t.Fatal(err)
				}
				first := doc.store.pending.items[0]
				first.Content = panicOnLen{first.Content}
				err := form.apply(doc, gap, fromAClient)
				if err == nil || errors.Is(err, ErrIntegrateScanBudget) || !errors.Is(err, ErrInvalidUpdate) ||
					!strings.Contains(err.Error(), "a parked struct that panics") {
					t.Fatalf("the apply that filled the gap returned %v, want ErrInvalidUpdate naming the panic", err)
				}
				if got := len(doc.store.pending.items); got != n-1 {
					t.Fatalf("%d items parked, want %d", got, n-1)
				}
			})
		}
	}
}

// A document applies an update with no bound as one without a budget does,
// when its budget sets no bound for the origin, and a bound that never trips
// places every item where no bound does.
func TestAnApplyWithNoBoundIsAsWithoutABudget(t *testing.T) {
	const l, n = 200, 20
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	update := originAtTheStartFrom(0, n)
	want := New()
	for _, u := range [][]byte{state, update} {
		if err := ApplyUpdateV1(want, u, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, form := range applyForms {
		for name, apply := range map[string]func(t *testing.T) *Doc{
			"zero for every origin": func(t *testing.T) *Doc {
				doc := New(WithIntegrateScanBudget(func(any) int { return 0 }))
				for _, u := range [][]byte{state, update} {
					if err := form.apply(doc, u, fromAClient); err != nil {
						t.Fatal(err)
					}
				}
				return doc
			},
			"relayed or loaded": func(t *testing.T) *Doc {
				doc := boundTo(t, state, 1)
				if err := form.apply(doc, update, nil); err != nil {
					t.Fatal(err)
				}
				return doc
			},
			"never trips": func(t *testing.T) *Doc {
				doc := boundTo(t, state, 1<<30)
				if err := form.apply(doc, update, fromAClient); err != nil {
					t.Fatal(err)
				}
				return doc
			},
		} {
			t.Run(form.name+"/"+name, func(t *testing.T) { requireSameState(t, apply(t), want) })
		}
	}
	for seed := range uint64(5) {
		state, keystrokes := concurrentKeystrokes(t, seed, 8, 30)
		merged, err := MergeUpdatesV1(keystrokes...)
		if err != nil {
			t.Fatal(err)
		}
		doc := boundTo(t, state, 1<<30)
		if err := ApplyUpdateV1(doc, merged, fromAClient); err != nil {
			t.Fatal(err)
		}
		if got, want := doc.GetText("t").ToString(), loadText(t, state, merged); got != want {
			t.Fatalf("seed %d: under a bound that never trips the keystrokes read\n%s\nwant\n%s", seed, got, want)
		}
	}
}

// A bounded document applies remote moves and subdocuments, even under a
// bound of one item.
func TestABoundedDocumentAppliesRemoteMovesAndSubdocuments(t *testing.T) {
	writer := New(WithClientID(3))
	array, m := writer.GetArray("a"), writer.GetMap("m")
	writer.Transact(func(txn *Transaction) { array.Push(txn, []any{1, 2, 3}) })
	writer.Transact(func(txn *Transaction) { array.Move(txn, 0, 3) })
	sub := New()
	writer.Transact(func(txn *Transaction) { m.Set(txn, "sub", sub) })
	update := EncodeStateAsUpdateV1(writer, nil)

	for _, form := range applyForms {
		t.Run(form.name, func(t *testing.T) {
			doc := New(WithIntegrateScanBudget(func(any) int { return 1 }))
			var added []*Doc
			doc.OnSubdocs(func(e SubdocsEvent) { added = append(added, e.Added...) })
			if err := form.apply(doc, update, fromAClient); err != nil {
				t.Fatal(err)
			}
			if got, want := fmt.Sprint(doc.GetArray("a").ToSlice()), fmt.Sprint(array.ToSlice()); got != want {
				t.Fatalf("the array is %s, want %s after the move", got, want)
			}
			if len(added) != 1 || added[0].GUID() != sub.GUID() {
				t.Fatalf("the subdocuments added are %v, want the one with guid %s", added, sub.GUID())
			}
		})
	}
}
