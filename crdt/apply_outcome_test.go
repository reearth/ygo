package crdt

import (
	"bytes"
	"testing"
)

// Tests for ApplyUpdateV1Outcome: what the commit of a remote apply
// integrated, and whether the update was applied with part of it parked.

// outcomeWriter is a writer whose every transaction is one update.
type outcomeWriter struct{ doc *Doc }

func (w outcomeWriter) insert(at int, s string) []byte {
	sv := w.doc.StateVector()
	text := w.doc.GetText("t")
	w.doc.Transact(func(txn *Transaction) { text.Insert(txn, at, s, nil) })
	return EncodeStateAsUpdateV1(w.doc, sv)
}

func (w outcomeWriter) delete(at, n int) []byte {
	sv := w.doc.StateVector()
	text := w.doc.GetText("t")
	w.doc.Transact(func(txn *Transaction) { text.Delete(txn, at, n) })
	return EncodeStateAsUpdateV1(w.doc, sv)
}

func applyOutcome(t *testing.T, doc *Doc, update []byte) ApplyOutcome {
	t.Helper()
	out, err := ApplyUpdateV1Outcome(doc, update, nil)
	if err != nil {
		t.Fatalf("the update was refused: %v", err)
	}
	return out
}

// updatesText is what a fresh document reads after applying updates in order.
func updatesText(t *testing.T, updates ...[]byte) string {
	t.Helper()
	doc := New()
	for _, u := range updates {
		if err := ApplyUpdateV1(doc, u, nil); err != nil {
			t.Fatalf("a fresh document refused an update: %v", err)
		}
	}
	return doc.GetText("t").ToString()
}

// Integrated is the very update the commit hands its OnUpdate observers, and
// is encoded for the caller when the document has none.
func TestUnit_ApplyOutcome_IntegratedIsTheCommitsOwnUpdate(t *testing.T) {
	w := outcomeWriter{New(WithClientID(5))}
	update := w.insert(0, "hello")

	observed := New(WithClientID(1))
	var seen []byte
	observed.OnUpdate(func(u []byte, _ any) { seen = u })
	out := applyOutcome(t, observed, update)
	if len(seen) == 0 || len(out.Integrated) == 0 || &seen[0] != &out.Integrated[0] {
		t.Fatalf("Integrated is not the slice OnUpdate was handed: %x against %x", out.Integrated, seen)
	}
	if out.Parked || out.Held || out.Landed != nil {
		t.Fatalf("an update that integrated whole reports %+v", out)
	}

	bare := New(WithClientID(1))
	if got := applyOutcome(t, bare, update); !bytes.Equal(got.Integrated, seen) {
		t.Fatalf("with no observer Integrated is %x, want the commit's update %x", got.Integrated, seen)
	}
}

// An update behind a gap of its own client parks: it integrates nothing and
// is parked. The update that fills the gap integrates both, its drain landing
// the parked one in the same commit, and leaves nothing parked.
func TestUnit_ApplyOutcome_AFillCarriesWhatItsDrainLanded(t *testing.T) {
	w := outcomeWriter{New(WithClientID(5))}
	first := w.insert(0, "ab")
	second := w.insert(2, "cd")

	room := New(WithClientID(1))
	parked := applyOutcome(t, room, second)
	if !parked.Parked || parked.Integrated != nil || parked.Held {
		t.Fatalf("an update behind a gap reports %+v, want parked with nothing integrated", parked)
	}
	fill := applyOutcome(t, room, first)
	if fill.Parked || fill.Held {
		t.Fatalf("the fill reports %+v, want nothing parked", fill)
	}
	if got := updatesText(t, fill.Integrated); got != "abcd" {
		t.Fatalf("the fill's Integrated reads %q, want the fill and what its drain landed, %q", got, "abcd")
	}
	if again := applyOutcome(t, room, second); again.Parked || !again.Held || again.Integrated != nil {
		t.Fatalf("the parked update sent again once it landed reports %+v, want held", again)
	}
}

// An update whose structs merge into a struct the queue already holds - the
// same parked edit sent again, and a longer run of it - is parked too, though
// the park record of its apply lists none of them.
func TestUnit_ApplyOutcome_AnUpdateMergedIntoTheQueueIsParked(t *testing.T) {
	w := outcomeWriter{New(WithClientID(5))}
	first := w.insert(0, "ab")
	sv := w.doc.StateVector()
	second := w.insert(2, "cd")
	third := w.insert(4, "ef")
	both := EncodeStateAsUpdateV1(w.doc, sv)

	room := New(WithClientID(1))
	applyOutcome(t, room, second)
	for name, u := range map[string][]byte{"the same update": second, "a longer run": both, "the run past it": third} {
		if out := applyOutcome(t, room, u); !out.Parked || out.Integrated != nil {
			t.Fatalf("%s, sent while the first parks, reports %+v, want parked", name, out)
		}
	}
	if fill := applyOutcome(t, room, first); fill.Parked || updatesText(t, fill.Integrated) != "abcdef" {
		t.Fatalf("the fill reports %+v reading %q, want all six characters integrated", fill, updatesText(t, fill.Integrated))
	}
}

// A delete of clocks the document does not hold parks with the update, which
// is parked; the update that brings those clocks applies the delete and
// leaves nothing parked.
func TestUnit_ApplyOutcome_AParkedDeleteIsParked(t *testing.T) {
	w := outcomeWriter{New(WithClientID(5))}
	insert := w.insert(0, "abcd")
	del := outcomeWriter{New(WithClientID(6))}
	if err := ApplyUpdateV1(del.doc, insert, nil); err != nil {
		t.Fatal(err)
	}
	cut := del.delete(1, 2)

	room := New(WithClientID(1))
	if out := applyOutcome(t, room, cut); !out.Parked || out.Integrated != nil {
		t.Fatalf("a delete of clocks the room lacks reports %+v, want parked", out)
	}
	fill := applyOutcome(t, room, insert)
	if fill.Parked {
		t.Fatalf("the insert the delete waited on reports %+v, want nothing parked", fill)
	}
	if got := room.GetText("t").ToString(); got != "ad" {
		t.Fatalf("the room reads %q, want the parked delete applied, %q", got, "ad")
	}
}

// Held and Landed are what ApplyUpdateV1Held reports: an update the document
// holds is held, integrates nothing and parks nothing, and an update that
// fails partway reports what it left in the document.
func TestUnit_ApplyOutcome_HeldAndLandedAreApplyUpdateV1Helds(t *testing.T) {
	w := outcomeWriter{New(WithClientID(5))}
	update := w.insert(0, "abc")

	room := New(WithClientID(1))
	applyOutcome(t, room, update)
	if out := applyOutcome(t, room, update); !out.Held || out.Parked || out.Integrated != nil {
		t.Fatalf("an update the room holds reports %+v, want held", out)
	}

	// The structs integrate before the cut-short delete set fails to decode.
	sv := w.doc.StateVector()
	text := w.doc.GetText("t")
	w.doc.Transact(func(txn *Transaction) {
		text.Insert(txn, 3, "def", nil)
		text.Delete(txn, 0, 1)
	})
	whole := EncodeStateAsUpdateV1(w.doc, sv)
	broken := whole[:len(whole)-1]
	twin := New(WithClientID(1))
	if err := ApplyUpdateV1(twin, update, nil); err != nil {
		t.Fatal(err)
	}
	wantLanded, wantErr := ApplyUpdateV1Landed(twin, broken, nil)
	out, err := ApplyUpdateV1Outcome(room, broken, nil)
	if err == nil || wantErr == nil {
		t.Fatalf("the cut-short update applied: %v, %v", err, wantErr)
	}
	if wantLanded == nil || !bytes.Equal(out.Landed, wantLanded) {
		t.Fatalf("the cut-short update reports Landed %x, ApplyUpdateV1Landed %x", out.Landed, wantLanded)
	}
	if out.Parked || out.Held {
		t.Fatalf("a failed apply reports %+v, want neither parked nor held", out)
	}
	if got := updatesText(t, update, out.Integrated); got != "abcdef" {
		t.Fatalf("what the failed apply integrated reads %q, want %q", got, "abcdef")
	}
}
