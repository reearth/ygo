package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Doc.SetDiscardRefusedDrain: a drain of the pending queue that a
// document takes back discards what the update that filled the gap released,
// instead of parking it again, and the apply says what it discarded and who
// parked it (DrainDiscardedError).

// discardScenario is newDrainScenario's document with the option on, the
// attacker's parked update applied with origin "attacker" once the option is
// set: client 4's "EVIL" after client 5's "y", which the document lacks,
// and, with deletes, client 4's deletion of that "y" too.
func discardScenario(t *testing.T, deletes bool) (*drainScenario, *Doc) {
	t.Helper()
	return discardScenarioBy(t, deletes, "attacker")
}

// discardScenarioBy is discardScenario with the attacker's update applied
// with origin.
func discardScenarioBy(t *testing.T, deletes bool, origin any) (*drainScenario, *Doc) {
	t.Helper()
	s := newDrainScenario(t)
	attacker := New(WithClientID(4))
	for _, u := range [][]byte{s.base, s.y} {
		require.NoError(t, ApplyUpdateV1(attacker, u, nil))
	}
	sv := attacker.StateVector()
	text := attacker.GetText("t")
	attacker.Transact(func(txn *Transaction) {
		text.Insert(txn, text.Len(), "EVIL", nil)
		if deletes {
			text.Delete(txn, text.Len()-5, 1) // the "y"
		}
	})
	doc := checkedDoc(t, s.base)
	doc.SetDiscardRefusedDrain(true)
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(attacker, sv), origin))
	require.Equal(t, 1, doc.PendingStats().Items, "EVIL parks behind client 5's gap")
	doc.SetApplyCheck(refuseEvil(doc.GetText("t")))
	return s, doc
}

// requireDiscarded returns err as a DrainDiscardedError, which reads as a
// drain refused while the update stood (ErrApplyCheckDrain) and wraps cause.
func requireDiscarded(t *testing.T, err, cause error) *DrainDiscardedError {
	t.Helper()
	var discarded *DrainDiscardedError
	require.ErrorAs(t, err, &discarded, "the apply returns a DrainDiscardedError")
	require.ErrorIs(t, err, ErrApplyCheckDrain)
	require.ErrorIs(t, err, cause)
	var refused *RefusedUpdateError
	assert.NotErrorAs(t, err, &refused, "the discard does not read as a refusal of the update")
	return discarded
}

// A drain the apply check refuses is taken back and what the fill released
// discarded: the parked struct and the parked deletion of the update's own
// clock. The update that filled the gap stands, the apply names what it
// discarded and who parked it, and the next update of the same client finds
// nothing left to drain, where off the refusal repeats with each of them.
func TestUnit_DiscardRefusedDrain_ARefusedDrainDiscardsWhatTheFillReleased(t *testing.T) {
	s, doc := discardScenario(t, true)
	require.Equal(t, 1, doc.PendingStats().DeleteRanges, "the deletion of the y parks too")

	discarded := requireDiscarded(t, ApplyUpdateV1(doc, s.y, "honest"), errCheckRefused)
	assert.Equal(t, 1, discarded.Structs)
	assert.Equal(t, 1, discarded.Ranges)
	assert.Equal(t, []any{"attacker"}, discarded.Senders)
	assert.Equal(t, "hello worldy", doc.GetText("t").ToString(), "the y stands, not deleted")
	assert.Equal(t, PendingStats{}, doc.PendingStats(), "nothing parked is left")

	require.NoError(t, ApplyUpdateV1(doc, s.z, "honest"), "nothing is left to drain, or to refuse")
	require.NoError(t, ApplyUpdateV1(s.control, s.y, nil))
	require.NoError(t, ApplyUpdateV1(s.control, s.z, nil))
	for _, pair := range [][2]*Doc{{doc, s.control}, {s.control, doc}} {
		held, err := DocContainsUpdateV1(pair[0], EncodeStateAsUpdateV1(pair[1], nil))
		require.NoError(t, err)
		assert.True(t, held, "the document is the control that never heard from the attacker")
	}
}

// Off, the same refusal parks the backlog again whole and returns the check's
// refusal alone, as before the option.
func TestUnit_DiscardRefusedDrain_OffParksTheBacklogAgain(t *testing.T) {
	s, doc := discardScenario(t, true)
	doc.SetDiscardRefusedDrain(false)

	err := ApplyUpdateV1(doc, s.y, "honest")
	requireDrainRefused(t, err)
	var discarded *DrainDiscardedError
	assert.NotErrorAs(t, err, &discarded)
	assert.Equal(t, 1, doc.PendingStats().Items, "EVIL is parked again")
	assert.Equal(t, 1, doc.PendingStats().DeleteRanges, "and so is the deletion")
	requireDrainRefused(t, ApplyUpdateV1(doc, s.z, "honest"))
}

// A parked deletion is discarded only below the clocks the fill released:
// its part past them stays parked, and lands with the update that brings
// those clocks. Each apply that parked what was discarded is named once.
func TestUnit_DiscardRefusedDrain_ADeleteRangeIsDiscardedBelowTheReleasedClocksOnly(t *testing.T) {
	s, doc := discardScenario(t, false)
	// Client 6 holds the y and the z, and deletes both.
	six := New(WithClientID(6))
	for _, u := range [][]byte{s.base, s.y, s.z} {
		require.NoError(t, ApplyUpdateV1(six, u, nil))
	}
	sv := six.StateVector()
	text := six.GetText("t")
	six.Transact(func(txn *Transaction) { text.Delete(txn, text.Len()-2, 2) })
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(six, sv), "six"))
	require.Equal(t, 1, doc.PendingStats().DeleteRanges)

	discarded := requireDiscarded(t, ApplyUpdateV1(doc, s.y, "honest"), errCheckRefused)
	assert.Equal(t, 1, discarded.Structs)
	assert.Equal(t, 1, discarded.Ranges, "the deletion of the y")
	assert.ElementsMatch(t, []any{"attacker", "six"}, discarded.Senders)
	assert.Equal(t, "hello worldy", doc.GetText("t").ToString())
	assert.Equal(t, PendingStats{DeleteRanges: 1}, doc.PendingStats(), "the deletion of the z stays parked")

	require.NoError(t, ApplyUpdateV1(doc, s.z, "honest"))
	assert.Equal(t, "hello worldy", doc.GetText("t").ToString(), "the z lands and its parked deletion with it")
	assert.Equal(t, PendingStats{}, doc.PendingStats())
}

// A drain past the scan bound is discarded as one the check refused is, with
// the bound's error as its cause.
func TestUnit_DiscardRefusedDrain_ADrainPastTheScanBoundIsDiscarded(t *testing.T) {
	const l, n, bound = 200, 20, 1_000
	doc := wholeOrNothing(t, EncodeStateAsUpdateV1(chainedList(t, l), nil), bound)
	doc.SetDiscardRefusedDrain(true)
	require.NoError(t, ApplyUpdateV1(doc, originAtTheStartFrom(1, n), "parker"))
	require.Equal(t, n, doc.PendingStats().Items)

	// The gap's item scans 199 items, which leaves the drain 801: four of the
	// parked items, which scan 200 each, before the fifth passes the bound.
	discarded := requireDiscarded(t, ApplyUpdateV1(doc, originAtTheStartFrom(0, 1), fromAClient), ErrIntegrateScanBudget)
	assert.Equal(t, n, discarded.Structs)
	assert.Zero(t, discarded.Ranges)
	assert.Equal(t, []any{"parker"}, discarded.Senders)
	assert.GreaterOrEqual(t, discarded.Scanned, 4*l, "the drain's scans before the bound stopped it")
	assert.LessOrEqual(t, discarded.Scanned, bound-l+1, "at most what the bound left the drain")
	assert.Equal(t, PendingStats{}, doc.PendingStats())
	assert.Equal(t, uint64(1), doc.StateVector().Clock(9), "the fill stands")
}

// A drain whose split passes the apply budget is discarded too, with the
// budget's error as its cause, and the fill stands with its own content.
func TestUnit_DiscardRefusedDrain_ADrainPastTheSplitBudgetIsDiscarded(t *testing.T) {
	const base = "hello world"
	writer := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(writer, baseState(t, base), nil))
	text := writer.GetText("t")
	held := writer.StateVector()
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 3, "x", nil) })
	first := EncodeStateAsUpdateV1(writer, held)
	afterFirst := writer.StateVector()
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 7, "y", nil) })
	second := EncodeStateAsUpdateV1(writer, afterFirst)

	doc := New(WithClientID(7), WithWholeOrNothingApplies(true))
	require.NoError(t, ApplyUpdateV1(doc, baseState(t, base), nil))
	doc.SetDiscardRefusedDrain(true)
	require.NoError(t, ApplyUpdateV1(doc, second, "parker"))
	require.Equal(t, 1, doc.PendingStats().Items)
	// "x" makes one split to land; the parked "y" then needs a second.
	doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Split: 1} })

	discarded := requireDiscarded(t, ApplyUpdateV1(doc, first, "filler"), ErrApplyBudget)
	assert.Equal(t, 1, discarded.Structs)
	assert.Equal(t, []any{"parker"}, discarded.Senders)
	assert.Equal(t, PendingStats{}, doc.PendingStats())
	assert.Equal(t, "helxlo world", doc.GetText("t").ToString(), "the fill's own character stands")
}

// Only what the fill released is discarded: a struct parked behind a gap the
// fill leaves open stays parked, and a park made before the option was set
// names nobody.
func TestUnit_DiscardRefusedDrain_WhatTheFillDoesNotReleaseStaysParked(t *testing.T) {
	s := newDrainScenario(t) // EVIL parked with no origin recorded
	doc := s.doc
	doc.SetDiscardRefusedDrain(true)
	other := New(WithClientID(8))
	for _, u := range [][]byte{s.base, s.y, s.z} {
		require.NoError(t, ApplyUpdateV1(other, u, nil))
	}
	sv := other.StateVector()
	text := other.GetText("t")
	other.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "after z", nil) })
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(other, sv), "other"))
	require.Equal(t, 2, doc.PendingStats().Items)
	doc.SetApplyCheck(refuseEvil(doc.GetText("t")))

	discarded := requireDiscarded(t, ApplyUpdateV1(doc, s.y, "honest"), errCheckRefused)
	assert.Equal(t, 1, discarded.Structs, "EVIL, which the y released")
	assert.Empty(t, discarded.Senders, "EVIL was parked before the option was set")
	assert.Equal(t, 1, doc.PendingStats().Items, "the struct behind the z stays parked")

	require.NoError(t, ApplyUpdateV1(doc, s.z, "honest"), "the z releases it, and the check takes it")
	assert.Equal(t, "hello worldyzafter z", doc.GetText("t").ToString())
	assert.Zero(t, doc.PendingStats().Items)
}

// An origin that cannot be a map key is named as it is, without a panic.
func TestUnit_DiscardRefusedDrain_AnOriginThatIsNoMapKeyIsNamed(t *testing.T) {
	unhashable := []string{"a peer"}
	s, doc := discardScenarioBy(t, true, unhashable)
	discarded := requireDiscarded(t, ApplyUpdateV1(doc, s.y, "honest"), errCheckRefused)
	assert.Equal(t, []any{unhashable, unhashable}, discarded.Senders,
		"one origin no map can hold is named for the struct and for the deletion")
}

// A parked deletion still names an origin when the record of who parked it
// keeps its pending range under another sender's: here a's frame parks every
// other clock past the y's and b's parks the range over them, the y's clock
// with it, so the record, past twice the one pending range, keeps that range
// under a, the first to touch it.
func TestUnit_DiscardRefusedDrain_ARangeKeptUnderAnotherSendersOriginIsNamed(t *testing.T) {
	s, doc := discardScenario(t, false)
	y := doc.StateVector().Clock(5)
	const m = 20
	singles := make([]DeleteRange, m)
	for j := range singles {
		singles[j] = DeleteRange{Clock: y + 1 + 2*uint64(j), Len: 1}
	}
	require.NoError(t, ApplyUpdateV1(doc, deleteSetUpdate(map[ClientID][]DeleteRange{5: singles}), "a"))
	require.NoError(t, ApplyUpdateV1(doc, deleteSetUpdate(map[ClientID][]DeleteRange{5: {{Clock: y, Len: 2*m + 1}}}), "b"))
	require.Equal(t, 1, doc.PendingStats().DeleteRanges)
	require.Equal(t, 1, recordedRanges(doc), "the record keeps the one pending range under one origin")

	discarded := requireDiscarded(t, ApplyUpdateV1(doc, s.y, "honest"), errCheckRefused)
	assert.Equal(t, 1, discarded.Ranges, "the deletion of the y")
	assert.ElementsMatch(t, []any{"attacker", "a"}, discarded.Senders)
	assert.Equal(t, PendingStats{DeleteRanges: 1}, doc.PendingStats(), "the deletions past the y stay parked")
}
