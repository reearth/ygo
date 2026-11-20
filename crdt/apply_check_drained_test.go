package crdt

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordDrained sets doc's apply check to one that passes every run and
// records, run by run, what its view says of whether it judges the drain
// (ApplyView.Drained).
func recordDrained(doc *Doc) *[]bool {
	runs := []bool{}
	doc.SetApplyCheck(func(_ any, view *ApplyView) error {
		runs = append(runs, view.Drained())
		return nil
	})
	return &runs
}

// ownClockParked is a document of client 7 holding state with a struct of
// client 4 parked behind client 7's first clock, which only the document
// itself writes, and then that clock written by the document in a
// transaction of its own, which drains nothing: the next remote apply's
// drain lands the parked struct, whatever that apply's update does.
func ownClockParked(t *testing.T, state []byte) *Doc {
	t.Helper()
	doc := checkedDoc(t, state)
	own := New(WithClientID(7))
	require.NoError(t, ApplyUpdateV1(own, state, nil))
	sv := own.StateVector()
	ownText := own.GetText("t")
	own.Transact(func(txn *Transaction) { ownText.Insert(txn, ownText.Len(), "x", nil) })
	written := EncodeStateAsUpdateV1(own, sv)

	other := New(WithClientID(4))
	require.NoError(t, ApplyUpdateV1(other, state, nil))
	require.NoError(t, ApplyUpdateV1(other, written, nil))
	sv = other.StateVector()
	otherText := other.GetText("t")
	other.Transact(func(txn *Transaction) { otherText.Insert(txn, otherText.Len(), "after", nil) })
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(other, sv), "other"))
	require.Equal(t, 1, doc.PendingStats().Items, "the struct after client 7's first clock parks")

	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "x", nil) })
	require.Equal(t, 1, doc.PendingStats().Items, "the document's own write drains nothing")
	return doc
}

// onlyParks is an update of client 6 holding state whose one struct waits
// on the client's first clock, which it leaves out: it only parks.
func onlyParks(t *testing.T, state []byte) []byte {
	t.Helper()
	six := New(WithClientID(6))
	require.NoError(t, ApplyUpdateV1(six, state, nil))
	text := six.GetText("t")
	six.Transact(func(txn *Transaction) { text.Insert(txn, 0, "p", nil) })
	sv := six.StateVector()
	six.Transact(func(txn *Transaction) { text.Insert(txn, 0, "q", nil) })
	return EncodeStateAsUpdateV1(six, sv)
}

// Each run of an apply check says whether it judges the apply's drain
// (ApplyView.Drained): false on the run of the update, true on the run of the
// drain. The one run of an apply whose update changed nothing - the
// document held all of it, or its structs only parked - and whose drain
// landed something is the drain's.
func TestUnit_ApplyCheck_EachRunSaysWhetherItJudgesTheDrain(t *testing.T) {
	for _, f := range applyFormats {
		t.Run(f.name, func(t *testing.T) {
			for _, c := range []struct {
				name string
				// doc is the document the update is applied to, and update
				// that update.
				doc  func(t *testing.T) (*Doc, []byte)
				runs []bool
				// parked is how many structs stay parked after the apply.
				parked int
			}{
				{"an update that fills the gap a struct parked behind", func(t *testing.T) (*Doc, []byte) {
					s := newDrainScenario(t)
					return s.doc, s.y
				}, []bool{false, true}, 0},
				{"an update the document already held, after its own write released a parked struct", func(t *testing.T) (*Doc, []byte) {
					base := checkBase(t)
					return ownClockParked(t, base), base
				}, []bool{true}, 0},
				{"an update whose structs only park, after the document's own write released a parked struct", func(t *testing.T) (*Doc, []byte) {
					base := checkBase(t)
					return ownClockParked(t, base), onlyParks(t, base)
				}, []bool{true}, 1},
				{"an update whose structs only park, with nothing to release", func(t *testing.T) (*Doc, []byte) {
					base := checkBase(t)
					return checkedDoc(t, base), onlyParks(t, base)
				}, []bool{}, 1},
				{"an update that changes the document, with nothing parked", func(t *testing.T) (*Doc, []byte) {
					s := newDrainScenario(t)
					return checkedDoc(t, s.base), s.y
				}, []bool{false}, 0},
			} {
				t.Run(c.name, func(t *testing.T) {
					doc, update := c.doc(t)
					runs := recordDrained(doc)
					require.NoError(t, applyIn(t, doc, f.v2, update))
					assert.Equal(t, c.runs, *runs, "what each run of the check says of whether it judges the drain")
					assert.Equal(t, c.parked, doc.PendingStats().Items)
					if slices.Contains(c.runs, true) {
						text := doc.GetText("t").ToString()
						assert.True(t, strings.Contains(text, "EVIL") || strings.Contains(text, "after"), "the drain landed the parked struct: %q", text)
					}
				})
			}
		})
	}
}
