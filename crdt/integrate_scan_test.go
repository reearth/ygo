package crdt

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"
)

// scanDoc is a document holding one string of 2n characters, written by client 7 in one
// transaction, so the whole string is one item until something splits it.
func scanDoc(t testing.TB, n int, opts ...DocOption) *Doc {
	t.Helper()
	doc := New(append([]DocOption{WithClientID(7)}, opts...)...)
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, strings.Repeat("a", 2*n), nil) })
	return doc
}

// leftOriginsOnly is an update of n one-character items from client 9, the i-th naming
// (7, 2i) of scanDoc's string as its left origin and no right origin. Each splits the
// string at its origin and integrates past the string's rest, so its conflict scan
// passes a piece or two, and every half it leaves merges back into the string at
// commit (tryMergeWithLefts).
func leftOriginsOnly(n int, withRightOrigins bool) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	for i := range n {
		info := wireString | flagHasOrigin
		if withRightOrigins {
			info |= flagHasRightOrigin
		}
		enc.WriteUint8(info)
		enc.WriteVarUint(7)
		enc.WriteVarUint(uint64(2 * i))
		if withRightOrigins {
			enc.WriteVarUint(7)
			enc.WriteVarUint(uint64(2*i + 1))
		}
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

// applyMeasured applies update to doc and reports the bytes the apply
// allocated, how long it took and the error.
func applyMeasured(doc *Doc, update []byte) (uint64, time.Duration, error) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := ApplyUpdateV1(doc, update, nil)
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, took, err
}

// An update that splits a long run many times, and leaves each half where it
// can merge back at commit, costs the run's length once rather than once for
// each half. Each of these n items names a left origin inside client 7's run
// and no right origin, so it splits the run there and then integrates past the
// run's rest; at commit every half merges back into the run
// (tryMergeWithLefts). Each merge appended the half to the run's whole text,
// and searched and shifted client 7's store slice: at n = 20,000 that
// allocated 440,969,280 bytes in one apply in v1.51.3-sami.4.
func TestASplitRunMergedBackAtCommitCostsItsLengthOnce(t *testing.T) {
	const n = 20_000
	doc := scanDoc(t, n)
	update := leftOriginsOnly(n, false)
	allocated, took, err := applyMeasured(doc, update)
	t.Logf("n=%d: err=%v, allocated %d bytes in %s", n, err, allocated, took)
	if err != nil {
		t.Fatal(err)
	}
	if allocated > 64<<20 {
		t.Fatalf("the apply allocated %d bytes, want under %d", allocated, 64<<20)
	}
	text := doc.GetText("t").ToString()
	if want := strings.Repeat("a", 2*n) + strings.Repeat("x", n); text != want {
		t.Fatalf("the text is %d characters, %q... - want the run, then every inserted character", len(text), text[:min(len(text), 40)])
	}
	if got := len(doc.store.clients[7]); got != 1 {
		t.Fatalf("client 7's run is %d items, want it merged back into 1", got)
	}
	// The document's own state reads back as the same document.
	copied := New()
	if err := ApplyUpdateV1(copied, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	if got := copied.GetText("t").ToString(); got != text {
		t.Fatal("the document's state decodes to a different text")
	}
}

// chainedList is a document whose text is l one-character items from clients 5
// and 6 in turn, each typed at the end after the other's, so no two neighbours
// merge and each item's left origin is its left neighbour.
func chainedList(t testing.TB, l int, opts ...DocOption) *Doc {
	t.Helper()
	a := New(append([]DocOption{WithClientID(5)}, opts...)...)
	b := New(append([]DocOption{WithClientID(6)}, opts...)...)
	at, bt := a.GetText("t"), b.GetText("t")
	for i := range l {
		from, to, text := a, b, at
		if i%2 == 1 {
			from, to, text = b, a, bt
		}
		sv := to.StateVector()
		from.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "c", nil) })
		if err := ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, sv), nil); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// originAtTheStart is an update of n one-character items from client 9, each
// naming the first item of chainedList's text as its left origin and no right
// origin. YATA places such an item by scanning right from its origin past every
// item whose own origin lies in what the scan has passed (Item.integrate), and
// in chainedList that is every item to the end, so each of these items scans
// the whole list.
func originAtTheStart(n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	for range n {
		enc.WriteUint8(wireString | flagHasOrigin)
		enc.WriteVarUint(5)
		enc.WriteVarUint(0)
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

// testScanBound is the scan bound the bounded documents of these tests take.
const testScanBound = 1 << 20

// observed counts the updates doc's OnUpdate observers are handed.
func observed(doc *Doc) *int {
	n := new(int)
	doc.OnUpdate(func([]byte, any) { *n++ })
	return n
}

// requireUnchanged requires doc to encode as before did and its text to read as
// text did, and its observers to have been handed nothing since updates was
// zero.
func requireUnchanged(t *testing.T, doc *Doc, before []byte, text string, updates *int) {
	t.Helper()
	if after := EncodeStateAsUpdateV1(doc, nil); string(after) != string(before) {
		t.Fatalf("the document's state changed: %d bytes before, %d after", len(before), len(after))
	}
	if got := doc.GetText("t").ToString(); got != text {
		t.Fatalf("the text changed from %d to %d characters", len(text), len(got))
	}
	if *updates != 0 {
		t.Fatalf("the observers were handed %d updates, want none", *updates)
	}
}

// On a document with a scan bound, the conflict scans of one remote update
// share the bound, so an update whose items each scan a long list is refused
// once they pass it, in time proportional to the bound rather than to the
// update's length times the list's, and the refusal leaves the document as it
// was and tells no observer: the items that integrated before the bound was
// reached are taken out again.
func TestARemoteUpdatesConflictScansAreBounded(t *testing.T) {
	const l, n = 5_000, 500
	doc := chainedList(t, l, WithMaxIntegrateScan(testScanBound))
	before, text := EncodeStateAsUpdateV1(doc, nil), doc.GetText("t").ToString()
	updates := observed(doc)
	allocated, took, err := applyMeasured(doc, originAtTheStart(n))
	t.Logf("l=%d n=%d: err=%v, allocated %d bytes in %s", l, n, err, allocated, took)
	if !errors.Is(err, ErrIntegrateScanBudget) || !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("the update was applied with err %v, want ErrIntegrateScanBudget", err)
	}
	if allocated > 16<<20 {
		t.Fatalf("the refused update allocated %d bytes, want under %d", allocated, 16<<20)
	}
	requireUnchanged(t, doc, before, text, updates)
	// The document goes on taking what an editor sends.
	peer := New(WithClientID(11))
	if err := ApplyUpdateV1(peer, before, nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	peerText := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { peerText.Insert(txn, 3, "typed", nil) })
	if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(peer, sv), nil); err != nil {
		t.Fatal(err)
	}
	if got, want := doc.GetText("t").ToString(), peerText.ToString(); got != want {
		t.Fatal("the document and the peer that typed into it diverged")
	}
}

// A document with a scan bound applies every remote update whole or not at all:
// an update it refuses partway, for its scans or for bytes that do not decode
// after structs that did, a deletion among them, leaves the document as it
// was, so it goes on converging with a peer that never saw the update.
func TestABoundedDocumentRollsBackAnUpdateItRefusesPartway(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 179))
	for round := range 40 {
		doc := New(WithClientID(1), WithMaxIntegrateScan(4_000))
		twin := New(WithClientID(2))
		text, twinText := doc.GetText("t"), twin.GetText("t")
		marks, twinMarks := doc.GetMap("m"), twin.GetMap("m")
		// A shared history from three writers, typed in turn.
		writers := []*Doc{New(WithClientID(3)), New(WithClientID(4)), New(WithClientID(5))}
		for step := range 60 {
			w := writers[step%len(writers)]
			for _, other := range []*Doc{doc, twin} {
				if err := ApplyUpdateV1(w, EncodeStateAsUpdateV1(other, w.StateVector()), nil); err != nil {
					t.Fatal(err)
				}
			}
			sv := w.StateVector()
			wt, wm := w.GetText("t"), w.GetMap("m")
			w.Transact(func(txn *Transaction) {
				if n := wt.Len(); n > 0 && rng.IntN(3) == 0 {
					at := rng.IntN(n)
					wt.Delete(txn, at, min(1+rng.IntN(3), n-at))
				}
				wt.Insert(txn, rng.IntN(wt.Len()+1), strings.Repeat("w", 1+rng.IntN(4)), nil)
				wm.Set(txn, fmt.Sprintf("k%d", rng.IntN(5)), step)
			})
			update := EncodeStateAsUpdateV1(w, sv)
			for _, d := range []*Doc{doc, twin} {
				if err := ApplyUpdateV1(d, update, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
		// A peer writes a long update - inserts, a deletion, map sets - and it
		// reaches the bounded document cut short, or past its bound.
		peer := New(WithClientID(9))
		if err := ApplyUpdateV1(peer, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
			t.Fatal(err)
		}
		sv := peer.StateVector()
		pt, pm := peer.GetText("t"), peer.GetMap("m")
		peer.Transact(func(txn *Transaction) {
			for i := range 30 {
				pt.Insert(txn, rng.IntN(pt.Len()+1), fmt.Sprint(i), nil)
				pm.Set(txn, fmt.Sprintf("k%d", rng.IntN(5)), -i)
			}
			if n := pt.Len(); n > 4 {
				pt.Delete(txn, 1, 3)
			}
		})
		update := EncodeStateAsUpdateV1(peer, sv)
		var refused []byte
		if round%2 == 0 {
			// Every struct decodes; the delete set does not.
			refused = append(update[:len(update)-1:len(update)-1], 0xff, 0xff, 0xff)
		} else {
			// Past the bound: the peer's inserts, which integrate first (an update
			// carries its clients highest first), then items of client 2 that each
			// scan the text from its first item, against a bound of one item.
			refused = mergedUpdate(t, update, originAtTheStartOf(text, 400))
			doc.SetMaxIntegrateScan(1)
		}
		before := EncodeStateAsUpdateV1(doc, nil)
		textBefore := text.ToString()
		updates := observed(doc)
		err := ApplyUpdateV1(doc, refused, nil)
		doc.SetMaxIntegrateScan(4_000)
		if err == nil {
			t.Fatalf("round %d: the bounded document applied the update", round)
		}
		requireUnchanged(t, doc, before, textBefore, updates)
		if a, b := scanMapJSON(t, marks), scanMapJSON(t, twinMarks); a != b {
			t.Fatalf("round %d: the map diverged from the twin's: %s and %s", round, a, b)
		}
		// Both go on to take the peer's whole update and agree.
		for _, d := range []*Doc{doc, twin} {
			if err := ApplyUpdateV1(d, update, nil); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if a, b := text.ToString(), twinText.ToString(); a != b {
			t.Fatalf("round %d: the text diverged from the twin's:\n%s\n%s", round, a, b)
		}
		if a, b := scanMapJSON(t, marks), scanMapJSON(t, twinMarks); a != b {
			t.Fatalf("round %d: the map diverged from the twin's: %s and %s", round, a, b)
		}
	}
}

// scanMapJSON is m as JSON.
func scanMapJSON(t *testing.T, m *YMap) string {
	t.Helper()
	out, err := m.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// originAtTheStartOf is originAtTheStart against text's own first item, from
// client 2.
func originAtTheStartOf(text *YText, n int) []byte {
	first := text.start
	for first != nil && first.Deleted {
		first = first.Right
	}
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(2)
	enc.WriteVarUint(0)
	for range n {
		enc.WriteUint8(wireString | flagHasOrigin)
		enc.WriteVarUint(uint64(first.ID.Client))
		enc.WriteVarUint(first.ID.Clock)
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0)
	return append([]byte(nil), enc.Bytes()...)
}

// mergedUpdate is one update carrying both of a and b.
func mergedUpdate(t *testing.T, a, b []byte) []byte {
	t.Helper()
	merged, err := MergeUpdatesV1(a, b)
	if err != nil {
		t.Fatal(err)
	}
	return merged
}

// A document with a scan bound refuses a remote update carrying a subdocument,
// whose registration reaches outside its lists, before integrating anything.
func TestABoundedDocumentRefusesASubdocumentFromARemoteUpdate(t *testing.T) {
	writer := New(WithClientID(3))
	m := writer.GetMap("m")
	writer.Transact(func(txn *Transaction) {
		m.Set(txn, "plain", 1)
		m.Set(txn, "sub", New())
	})
	update := EncodeStateAsUpdateV1(writer, nil)
	bounded := New(WithMaxIntegrateScan(testScanBound))
	updates := observed(bounded)
	if err := ApplyUpdateV1(bounded, update, nil); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("the bounded document applied a subdocument with err %v", err)
	}
	if *updates != 0 || len(bounded.StateVector()) != 0 {
		t.Fatalf("the refused update left state %v and %d observed updates", bounded.StateVector(), *updates)
	}
	if err := ApplyUpdateV1(New(), update, nil); err != nil {
		t.Fatalf("an unbounded document refused the subdocument: %v", err)
	}
}

// The V2 form of a refused update is refused the same way.
func TestABoundedDocumentRefusesAV2UpdateWhoseScansPassTheBound(t *testing.T) {
	const l, n = 5_000, 500
	doc := chainedList(t, l, WithMaxIntegrateScan(testScanBound))
	before, text := EncodeStateAsUpdateV1(doc, nil), doc.GetText("t").ToString()
	updates := observed(doc)
	v2, err := UpdateV1ToV2(originAtTheStart(n))
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdateV2(doc, v2, nil); !errors.Is(err, ErrIntegrateScanBudget) {
		t.Fatalf("the V2 update was applied with err %v, want ErrIntegrateScanBudget", err)
	}
	requireUnchanged(t, doc, before, text, updates)
}

// parkedBehindAGap is originAtTheStart from client 99 starting at clock 1, so
// every item waits in the pending queue for client 99's clock 0.
func parkedBehindAGap(n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(99)
	enc.WriteVarUint(1)
	for range n {
		enc.WriteUint8(wireString | flagHasOrigin)
		enc.WriteVarUint(5)
		enc.WriteVarUint(0)
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0)
	return append([]byte(nil), enc.Bytes()...)
}

// fillTheGap is client 99's clock 0: one character appended after last.
func fillTheGap(last *Item) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteVarUint(99)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(uint64(last.ID.Client))
	enc.WriteVarUint(last.lastID().Clock)
	enc.WriteVarString("f")
	enc.WriteVarUint(0)
	return append([]byte(nil), enc.Bytes()...)
}

// Items an earlier update parked are not charged to the update that fills
// their gap: their drain has an allowance of its own, and a drain that passes
// it is taken back and the items parked again as they were, while the filling
// update applies.
func TestAnUpdateThatFillsAGapIsNotChargedWhatWasParkedBehindIt(t *testing.T) {
	const l, n = 5_000, 500
	doc := chainedList(t, l, WithMaxIntegrateScan(testScanBound))
	twin := chainedList(t, l)
	text := doc.GetText("t")
	if err := ApplyUpdateV1(doc, parkedBehindAGap(n), nil); err != nil {
		t.Fatalf("parking the items: %v", err)
	}
	if parked := doc.PendingStats().Items; parked != n {
		t.Fatalf("%d items are parked, want %d", parked, n)
	}
	last := text.start
	for last.Right != nil {
		last = last.Right
	}
	fill := fillTheGap(last)
	if err := ApplyUpdateV1(doc, fill, nil); err != nil {
		t.Fatalf("the update filling the gap was refused: %v", err)
	}
	if err := ApplyUpdateV1(twin, fill, nil); err != nil {
		t.Fatal(err)
	}
	if parked := doc.PendingStats().Items; parked != n {
		t.Fatalf("%d items are parked, want the %d parked items parked again", parked, n)
	}
	if a, b := text.ToString(), twin.GetText("t").ToString(); a != b {
		t.Fatal("the document holds more than the update that filled the gap")
	}
	if got := doc.StateVector().Clock(99); got != 1 {
		t.Fatalf("client 99's clock is %d, want 1", got)
	}
}

// A bound limits what one remote update may scan, not a document's history:
// two updates that each scan most of the bound are both taken, and the state
// they leave loads into a document with no bound, after which a bound can be
// set for what arrives next.
func TestABoundedDocumentsStateLoadsWhereverItsHistoryPassesTheBound(t *testing.T) {
	const l, n = 2_000, 300
	doc := chainedList(t, l, WithMaxIntegrateScan(400*l))
	for range 2 {
		before := doc.StateVector().Clock(9)
		enc := encoding.NewEncoder()
		enc.WriteVarUint(1)
		enc.WriteVarUint(uint64(n))
		enc.WriteVarUint(9)
		enc.WriteVarUint(before)
		for range n {
			enc.WriteUint8(wireString | flagHasOrigin)
			enc.WriteVarUint(5)
			enc.WriteVarUint(0)
			enc.WriteVarString("x")
		}
		enc.WriteVarUint(0)
		if err := ApplyUpdateV1(doc, enc.Bytes(), nil); err != nil {
			t.Fatalf("an update within the bound was refused: %v", err)
		}
	}
	state := EncodeStateAsUpdateV1(doc, nil)
	loaded := New()
	if err := ApplyUpdateV1(loaded, state, nil); err != nil {
		t.Fatalf("the state did not load into a document with no bound: %v", err)
	}
	loaded.SetMaxIntegrateScan(400 * l)
	if a, b := loaded.GetText("t").ToString(), doc.GetText("t").ToString(); a != b {
		t.Fatal("the loaded document reads differently")
	}
}

// The same items with their right origins set scan nothing, and the update applies:
// the bound counts the items a scan passes, not the items an update carries.
func TestAnUpdateWhoseItemsScanNothingIsNotBounded(t *testing.T) {
	const n = 20_000
	doc := scanDoc(t, n)
	allocated, took, err := applyMeasured(doc, leftOriginsOnly(n, true))
	t.Logf("n=%d with right origins: err=%v, allocated %d bytes in %s", n, err, allocated, took)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.StateVector().Clock(9); got != n {
		t.Fatalf("client 9's clock is %d, want %d", got, n)
	}
	if got := doc.GetText("t").Len(); got != 3*n {
		t.Fatalf("the text holds %d characters, want %d", got, 3*n)
	}
}

// Concurrent inserts at one position from many clients scan each other, as YATA
// requires, and converge to one order whichever order they arrive in; the bound is far
// above what they pass.
func TestConcurrentInsertsAtOnePositionStillConverge(t *testing.T) {
	const clients = 200
	base := New(WithClientID(1))
	baseText := base.GetText("t")
	base.Transact(func(txn *Transaction) { baseText.Insert(txn, 0, "ab", nil) })
	state := EncodeStateAsUpdateV1(base, nil)
	updates := make([][]byte, 0, clients)
	for c := range clients {
		peer := New(WithClientID(ClientID(100 + c)))
		if err := ApplyUpdateV1(peer, state, nil); err != nil {
			t.Fatal(err)
		}
		sv := peer.StateVector()
		peerText := peer.GetText("t")
		peer.Transact(func(txn *Transaction) { peerText.Insert(txn, 1, strings.Repeat("x", 1+c%3), nil) })
		updates = append(updates, EncodeStateAsUpdateV1(peer, sv))
	}
	forward, backward := New(), New()
	for _, doc := range []*Doc{forward, backward} {
		if err := ApplyUpdateV1(doc, state, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := range updates {
		if err := ApplyUpdateV1(forward, updates[i], nil); err != nil {
			t.Fatal(err)
		}
		if err := ApplyUpdateV1(backward, updates[len(updates)-1-i], nil); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := forward.GetText("t").ToString(), backward.GetText("t").ToString(); a != b {
		t.Fatalf("the documents diverged:\n%s\n%s", a, b)
	}
}

// A document has no scan bound unless it is given one, and lifted, a bounded
// document takes the same update in full; a local transaction is never bounded.
func TestTheConflictScanBoundIsSetOnlyWhereAsked(t *testing.T) {
	const l, n = 2_000, 200
	for _, doc := range []*Doc{chainedList(t, l), chainedList(t, l, WithMaxIntegrateScan(-1))} {
		if err := ApplyUpdateV1(doc, originAtTheStart(n), nil); err != nil {
			t.Fatal(err)
		}
		if got := doc.StateVector().Clock(9); got != n {
			t.Fatalf("client 9's clock is %d, want %d", got, n)
		}
	}
	bounded := chainedList(t, l)
	bounded.SetMaxIntegrateScan(10 * l)
	if err := ApplyUpdateV1(bounded, originAtTheStart(n), nil); !errors.Is(err, ErrIntegrateScanBudget) {
		t.Fatalf("a bound of %d items applied the update with err %v", 10*l, err)
	}
	text := bounded.GetText("t")
	bounded.Transact(func(txn *Transaction) { text.Insert(txn, 1, "y", nil) })
	if got := text.Len(); got != l+1 {
		t.Fatalf("the text holds %d characters after a local insert, want %d", got, l+1)
	}
}

// parkedDeletes is an update of no structs whose delete set names n one-clock
// ranges of client, every other clock from 0, which a document that never heard
// from client parks, every range, as pending deletes.
func parkedDeletes(client ClientID, n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // clients
	enc.WriteVarUint(1) // delete set: one client
	enc.WriteVarUint(uint64(client))
	enc.WriteVarUint(uint64(n))
	for i := range n {
		enc.WriteVarUint(uint64(2 * i))
		enc.WriteVarUint(1)
	}
	return append([]byte(nil), enc.Bytes()...)
}

// typedInto is the update of one character a peer of state types into its
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

// A bounded document applies a keystroke for what it costs an unbounded one,
// whatever earlier updates left parked: the apply's journal keeps the parked
// items, the clocks they wait on and the pending deletes as they are, by
// reference, rather than copying them so a refusal can restore them. In
// v1.51.3-sami.5 it copied all three on every apply, and the drain copied the
// pending deletes again: with 100,000 parked items the keystroke allocated
// 806,264 bytes on the bounded document against 1,608 on the unbounded one,
// and with 100,000 parked delete ranges 4,821,112 against 1,607,624 (the
// unbounded drain's own retry of the pending deletes).
func TestABoundedDocumentsApplyCostsNothingForWhatIsParked(t *testing.T) {
	const l, n = 100, 100_000
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	keystroke := typedInto(t, state)
	for _, parked := range []struct {
		name   string
		update []byte
	}{
		{"items", parkedBehindAGap(n)},
		{"delete ranges", parkedDeletes(98, n)},
	} {
		t.Run(parked.name, func(t *testing.T) {
			var allocated [2]uint64
			for i, opts := range [][]DocOption{nil, {WithMaxIntegrateScan(testScanBound)}} {
				doc := New(opts...)
				for _, update := range [][]byte{state, parked.update} {
					if err := ApplyUpdateV1(doc, update, nil); err != nil {
						t.Fatal(err)
					}
				}
				if stats := doc.PendingStats(); stats.Items+stats.DeleteRanges != n {
					t.Fatalf("%d items and %d delete ranges are parked, want %d in all", stats.Items, stats.DeleteRanges, n)
				}
				var took time.Duration
				var err error
				allocated[i], took, err = applyMeasured(doc, keystroke)
				if err != nil {
					t.Fatal(err)
				}
				if got := doc.StateVector().Clock(11); got != 1 {
					t.Fatalf("client 11's clock is %d, want the keystroke applied", got)
				}
				t.Logf("bounded=%t: allocated %d bytes in %s", i == 1, allocated[i], took)
			}
			if unbounded, bounded := allocated[0], allocated[1]; bounded > unbounded+64<<10 {
				t.Fatalf("with %d %s parked, the keystroke allocated %d bytes on the bounded document and %d on the unbounded one, want within %d of it",
					n, parked.name, bounded, unbounded, 64<<10)
			}
		})
	}
}

// parkingAndBroken is an update that parks items of its own before its delete
// set fails to decode: client 99's clocks from n+2, behind a gap at n+1, and
// one item of client 76 whose left origin is client 77's clock 0, which adds
// client 77 to the clocks the pending queue waits on.
func parkingAndBroken(n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(2) // clients
	enc.WriteVarUint(3)
	enc.WriteVarUint(99)
	enc.WriteVarUint(uint64(n + 2))
	for range 3 {
		enc.WriteUint8(wireString | flagHasOrigin)
		enc.WriteVarUint(5)
		enc.WriteVarUint(0)
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(1)
	enc.WriteVarUint(76)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(77)
	enc.WriteVarUint(0)
	enc.WriteVarString("x")
	enc.WriteUint8(0xff) // delete set: a client count cut short
	enc.WriteUint8(0xff)
	enc.WriteUint8(0xff)
	return append([]byte(nil), enc.Bytes()...)
}

// A bounded document that refuses an update leaves what earlier updates parked
// as it was: the parked items, the clients they wait on and the pending
// deletes, though the refused update parked items of its own into the same
// queue before it was refused. So it goes on like an unbounded twin that never
// saw the update: an update that fills every gap applies the same on both.
func TestABoundedDocumentsRefusalLeavesWhatIsParkedAsItWas(t *testing.T) {
	const l, n, k = 100, 50, 40
	state := EncodeStateAsUpdateV1(chainedList(t, l), nil)
	doc := New(WithClientID(1), WithMaxIntegrateScan(testScanBound))
	twin := New(WithClientID(2))
	for _, d := range []*Doc{doc, twin} {
		for _, update := range [][]byte{state, parkedBehindAGap(n), parkedDeletes(98, k)} {
			if err := ApplyUpdateV1(d, update, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	text := doc.GetText("t")
	if stats := doc.PendingStats(); stats.Items != n || stats.DeleteRanges != k {
		t.Fatalf("%d items and %d delete ranges are parked, want %d and %d", stats.Items, stats.DeleteRanges, n, k)
	}
	// On a document with no bound, which keeps what an update did before it
	// failed, the first refused update leaves its items parked.
	keeps := New()
	for _, update := range [][]byte{state, parkedBehindAGap(n), parkingAndBroken(n)} {
		_ = ApplyUpdateV1(keeps, update, nil)
	}
	if stats := keeps.PendingStats(); stats.Items != n+4 || fmt.Sprint(stats.MissingFor) != "[77 99]" {
		t.Fatalf("the update that fails to decode parked %d items waiting on %v, want 4 waiting on [77 99]", stats.Items-n, stats.MissingFor)
	}
	for _, refusal := range []struct {
		name   string
		update []byte
		bound  int
	}{
		{"a delete set that does not decode, after items it parked", parkingAndBroken(n), testScanBound},
		{"scans past the bound", originAtTheStartOf(text, 400), 1},
	} {
		before, textBefore := EncodeStateAsUpdateV1(doc, nil), text.ToString()
		stats := fmt.Sprintf("%+v", doc.PendingStats())
		updates := observed(doc)
		doc.SetMaxIntegrateScan(refusal.bound)
		err := ApplyUpdateV1(doc, refusal.update, nil)
		doc.SetMaxIntegrateScan(testScanBound)
		if err == nil {
			t.Fatalf("%s: the bounded document applied the update", refusal.name)
		}
		requireUnchanged(t, doc, before, textBefore, updates)
		if got := fmt.Sprintf("%+v", doc.PendingStats()); got != stats {
			t.Fatalf("%s: the pending queue is %s, want %s as it was", refusal.name, got, stats)
		}
		if got, want := fmt.Sprintf("%+v", doc.PendingStats()), fmt.Sprintf("%+v", twin.PendingStats()); got != want {
			t.Fatalf("%s: the pending queue is %s, the twin's %s", refusal.name, got, want)
		}
		if a, b := EncodeStateAsUpdateV1(doc, nil), EncodeStateAsUpdateV1(twin, nil); string(a) != string(b) {
			t.Fatalf("%s: the document's state, parked items included, differs from the twin's", refusal.name)
		}
	}
	// Client 99's clock 0, and client 98's clocks every pending delete names.
	last := text.start
	for last.Right != nil {
		last = last.Right
	}
	writer := New(WithClientID(98))
	if err := ApplyUpdateV1(writer, state, nil); err != nil {
		t.Fatal(err)
	}
	sv := writer.StateVector()
	written := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { written.Insert(txn, 0, strings.Repeat("d", 2*k), nil) })
	fill := mergedUpdate(t, fillTheGap(last), EncodeStateAsUpdateV1(writer, sv))
	for _, d := range []*Doc{doc, twin} {
		if err := ApplyUpdateV1(d, fill, nil); err != nil {
			t.Fatal(err)
		}
	}
	if stats := doc.PendingStats(); stats.Items != 0 || stats.DeleteRanges != 0 {
		t.Fatalf("after the update filling every gap, %d items and %d delete ranges are still parked", stats.Items, stats.DeleteRanges)
	}
	if a, b := EncodeStateAsUpdateV1(doc, nil), EncodeStateAsUpdateV1(twin, nil); string(a) != string(b) {
		t.Fatal("after the update filling every gap, the document's state differs from the twin's")
	}
	if a, b := text.ToString(), twin.GetText("t").ToString(); a != b {
		t.Fatalf("after the update filling every gap, the text diverged from the twin's:\n%s\n%s", a, b)
	}
}
