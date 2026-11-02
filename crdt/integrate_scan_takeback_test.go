package crdt

import (
	"errors"
	"fmt"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// boundedScans is what the conflict scans of doc's apply of update counted, on
// a document with a scan bound: the drain's alone when the apply drained the
// pending queue, since the drain's allowance starts again from zero.
func boundedScans(t *testing.T, doc *Doc, update []byte) int {
	t.Helper()
	scanned := -1
	stop := doc.OnAfterTransaction(func(txn *Transaction) {
		scanned = 0
		if txn.boundedApply != nil {
			scanned = txn.boundedApply.scanned
		}
	})
	defer stop()
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatal(err)
	}
	if scanned < 0 {
		t.Fatal("the apply committed no transaction")
	}
	return scanned
}

// itemAfter is one string item of client at clock, with origin as its left
// origin and no right origin.
func itemAfter(client ClientID, clock uint64, origin ID, s string) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(1)
	enc.WriteVarUint(uint64(client))
	enc.WriteVarUint(clock)
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(uint64(origin.Client))
	enc.WriteVarUint(origin.Clock)
	enc.WriteVarString(s)
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

// A bounded document's drain that passes its allowance after it integrated one
// peer's parked item and split it for another peer's is taken back, and the
// queue is parked again as the drain found it: the pending stats do not change,
// a third peer's parked item stays, and the encoded state, parked items
// included, reads as an unbounded twin's. The update that filled the gap
// stands. An apply that brings no client the queue waits on does not drain it
// again. An apply that brings one, with a bound that holds the drain, or the
// sender sending its parked item again, integrates the parked items, and the
// document's state then equals the twin's.
func TestUnit_IntegrateScan_TakenBackDrainParksTheQueueAgainAsItFoundIt(t *testing.T) {
	const l = 2_000
	parked := [][]byte{
		itemAfter(99, 1, ID{Client: 5, Clock: 0}, "abc"), // behind client 99's clock 0
		itemAfter(98, 0, ID{Client: 99, Clock: 2}, "r"),  // the middle of client 99's item
		itemAfter(76, 0, ID{Client: 77, Clock: 0}, "z"),  // waits on client 77
	}
	lastOf := func(doc *Doc) *Item {
		last := doc.GetText("t").start
		for last.Right != nil {
			last = last.Right
		}
		return last
	}
	// end is the end of a text, where every item these updates carry lands.
	end := func(text string) string { return text[max(0, len(text)-12):] }
	park := func(doc *Doc, updates ...[]byte) {
		t.Helper()
		for _, update := range updates {
			if err := ApplyUpdateV1(doc, update, "park"); err != nil {
				t.Fatal(err)
			}
		}
		if got := doc.StateVector().Clock(99); got != 0 {
			t.Fatalf("client 99's clock is %d after parking, want 0", got)
		}
	}
	// What the drain of the update filling client 99's gap counts, with
	// updates parked before it, under a bound that holds it.
	filling := func(updates ...[]byte) int {
		doc := chainedList(t, l, WithMaxIntegrateScan(testScanBound))
		park(doc, updates...)
		return boundedScans(t, doc, fillTheGap(lastOf(doc)))
	}
	if own := filling(); own != 0 {
		t.Fatalf("the filling update alone passed %d items, want 0", own)
	}
	// The drain's allowance holds client 99's item and runs out in the scan
	// of client 98's, after the split.
	limit := filling(parked[0])
	if both := filling(parked[:2]...); both <= limit {
		t.Fatalf("the drain of both items passed %d items, want more than the %d of client 99's alone", both, limit)
	}

	for _, then := range []struct {
		name   string
		update []byte
	}{
		{"an apply bringing client 77", itemAfter(77, 0, ID{Client: 50, Clock: 0}, "w")},
		{"the parked item's sender sending it again", parked[0]},
	} {
		t.Run(then.name, func(t *testing.T) {
			doc, twin := chainedList(t, l, WithMaxIntegrateScan(limit)), chainedList(t, l)
			park(doc, parked...)
			park(twin, parked...)
			stats := fmt.Sprintf("%+v", doc.PendingStats())
			fill := fillTheGap(lastOf(doc))
			if got := boundedScans(t, doc, fill); got <= limit {
				t.Fatalf("the filling apply's drain counted %d items, want it taken back past %d", got, limit)
			}
			if err := ApplyUpdateV1(twin, fill, nil); err != nil {
				t.Fatal(err)
			}
			if got := doc.StateVector().Clock(99); got != 1 {
				t.Fatalf("client 99's clock is %d, want 1: the drain taken back", got)
			}
			if got := fmt.Sprintf("%+v", doc.PendingStats()); got != stats {
				t.Fatalf("after the drain was taken back the pending queue is %s, want %s as it was", got, stats)
			}
			copied := New()
			if err := ApplyUpdateV1(copied, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
				t.Fatal(err)
			}
			if a, b := copied.GetText("t").ToString(), twin.GetText("t").ToString(); a != b {
				t.Fatalf("the document's state, parked items included, reads differently from the twin's: ends %q, the twin's %q", end(a), end(b))
			}

			unrelated := itemAfter(50, 0, ID{Client: 99, Clock: 0}, "u")
			if got := boundedScans(t, doc, unrelated); got != 0 {
				t.Fatalf("an apply bringing no client the queue waits on counted %d items, want 0", got)
			}
			if err := ApplyUpdateV1(twin, unrelated, nil); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%+v", doc.PendingStats()); got != stats {
				t.Fatalf("after an apply bringing no client the queue waits on the pending queue is %s, want %s", got, stats)
			}

			doc.SetMaxIntegrateScan(testScanBound)
			for _, d := range []*Doc{doc, twin} {
				if err := ApplyUpdateV1(d, then.update, "more"); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := fmt.Sprintf("%+v", doc.PendingStats()), fmt.Sprintf("%+v", twin.PendingStats()); got != want {
				t.Fatalf("the pending queue is %s, the twin's %s", got, want)
			}
			if a, b := doc.GetText("t").ToString(), twin.GetText("t").ToString(); a != b {
				t.Fatalf("the text diverged from the twin's: ends %q, the twin's %q", end(a), end(b))
			}
			if a, b := EncodeStateAsUpdateV1(doc, nil), EncodeStateAsUpdateV1(twin, nil); string(a) != string(b) {
				t.Fatal("the document's state, parked items included, differs from the twin's")
			}
		})
	}
}

// A bounded document rolls a refused update back whole, so ApplyUpdateV1Landed
// reports nothing landed: a caller that relays what a failed apply integrated
// relays nothing, and the document is as it was.
func TestUnit_IntegrateScan_RefusedUpdateLandsNothing(t *testing.T) {
	const l, n = 5_000, 500
	doc := chainedList(t, l, WithMaxIntegrateScan(testScanBound))
	before, text := EncodeStateAsUpdateV1(doc, nil), doc.GetText("t").ToString()

	// A deletion of items the document holds rides with the refused insert,
	// so the refused apply both integrated structs and deleted some before
	// its scans passed the bound.
	peer := New(WithClientID(11))
	if err := ApplyUpdateV1(peer, before, nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	peerText := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { peerText.Delete(txn, 0, 3) })
	update := mergedUpdate(t, EncodeStateAsUpdateV1(peer, sv), originAtTheStart(n))

	updates := observed(doc)
	landed, err := ApplyUpdateV1Landed(doc, update, nil)
	if !errors.Is(err, ErrIntegrateScanBudget) {
		t.Fatalf("the update was applied with err %v, want ErrIntegrateScanBudget", err)
	}
	if landed != nil {
		t.Fatalf("the refused update landed %d bytes, want nil", len(landed))
	}
	requireUnchanged(t, doc, before, text, updates)
}

// The drain of the pending queue counts against an allowance of its own, so an
// update whose own scans and whose drain each fit the bound applies with its
// parked item, though the two together pass the bound.
func TestUnit_IntegrateScan_DrainStartsAFreshAllowance(t *testing.T) {
	const l = 2_000
	first := ID{Client: 5, Clock: 0}
	fill := itemAfter(99, 0, first, "f")   // client 99's clock 0, scanning the list
	parked := itemAfter(99, 1, first, "p") // behind it, scanning the list again
	// apply applies updates in order and the last of them under bound.
	apply := func(bound int, updates ...[]byte) (*Doc, error) {
		doc := chainedList(t, l, WithMaxIntegrateScan(testScanBound))
		for _, update := range updates[:len(updates)-1] {
			if err := ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatal(err)
			}
		}
		doc.SetMaxIntegrateScan(bound)
		return doc, ApplyUpdateV1(doc, updates[len(updates)-1], nil)
	}
	// The smallest bound under which the last update applies, found by bisection.
	smallest := func(updates ...[]byte) int {
		lo, hi := 1, 4*l
		for lo < hi {
			mid := (lo + hi) / 2
			if _, err := apply(mid, updates...); err == nil {
				hi = mid
			} else if errors.Is(err, ErrIntegrateScanBudget) {
				lo = mid + 1
			} else {
				t.Fatal(err)
			}
		}
		return lo
	}
	own := smallest(fill)
	drain := smallest(fill, parked) // the parked item's scans, applied directly
	bound := max(own, drain)
	if own+drain <= bound {
		t.Fatalf("the fill scans %d items and the parked item %d, want them together past the bound %d", own, drain, bound)
	}

	doc, err := apply(bound, parked, fill)
	if err != nil {
		t.Fatalf("the filling update was refused: %v", err)
	}
	if stats := doc.PendingStats(); stats.Items != 0 {
		t.Fatalf("%d items are still parked, want the drain to apply the parked item", stats.Items)
	}
	if got := doc.StateVector().Clock(99); got != 2 {
		t.Fatalf("client 99's clock is %d, want 2", got)
	}
}

// Every item a bounded scan passes counts, including an item of another key it
// steps over: an XML child's scan past the element's attributes counts each.
func TestUnit_IntegrateScan_SkippedKeyItemsCount(t *testing.T) {
	const attributes = 20
	base := New(WithClientID(100))
	fragment := base.GetXmlFragment("x")
	base.Transact(func(txn *Transaction) { fragment.InsertElement(txn, 0, NewYXmlElement("e")) })
	initial := EncodeStateAsUpdateV1(base, nil)
	edit := func(client ClientID, change func(*Transaction, *YXmlElement)) []byte {
		doc := New(WithClientID(client))
		if err := ApplyUpdateV1(doc, initial, nil); err != nil {
			t.Fatal(err)
		}
		sv := doc.StateVector()
		element := doc.GetXmlFragment("x").Children()[0].(*YXmlElement)
		doc.Transact(func(txn *Transaction) { change(txn, element) })
		return EncodeStateAsUpdateV1(doc, sv)
	}
	keys := edit(9, func(txn *Transaction, el *YXmlElement) {
		for i := range attributes {
			el.SetAttribute(txn, fmt.Sprintf("k%d", i), "v")
		}
	})
	child := edit(2, func(txn *Transaction, el *YXmlElement) { el.InsertElement(txn, 0, NewYXmlElement("c")) })

	for _, bound := range []int{attributes - 1, attributes} {
		doc := New()
		for _, update := range [][]byte{initial, keys} {
			if err := ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatal(err)
			}
		}
		doc.SetMaxIntegrateScan(bound)
		err := ApplyUpdateV1(doc, child, nil)
		if bound < attributes {
			if !errors.Is(err, ErrIntegrateScanBudget) {
				t.Fatalf("under a bound of %d the child applied with err %v, want ErrIntegrateScanBudget", bound, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("under a bound of %d the child was refused: %v", bound, err)
		}
		element := doc.GetXmlFragment("x").Children()[0].(*YXmlElement)
		if got := len(element.Children()); got != 1 {
			t.Fatalf("the element has %d children, want the child applied", got)
		}
	}
}
