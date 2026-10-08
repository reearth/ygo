package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// squashRuns and RunGC merge two adjacent same-client items only when the right
// one's Origin is the left one's last character (Yjs Item.mergeWith). No client
// produces a run that breaks only that condition by editing, so these tests
// write the update themselves.

// crossedOriginRun builds "XYab": "X" (5:0), "Y" (1:0, origin X), "a" (3:0,
// origin Y) and "b" (3:1), where b's Origin is rewritten to X before encoding.
// YATA still places b after a, so a and b are adjacent, clock-contiguous and
// share a right origin (none), but b's Origin is not a. It returns the state
// without b, the update carrying only b, the whole state, and X's state.
func crossedOriginRun(t *testing.T) (withoutB, onlyB, whole, x []byte) {
	t.Helper()
	d5 := New(WithClientID(5))
	t5 := d5.GetText("t")
	d5.Transact(func(txn *Transaction) { t5.Insert(txn, 0, "X", nil) })
	x = EncodeStateAsUpdateV1(d5, nil)

	d1 := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(d1, x, nil))
	t1 := d1.GetText("t")
	d1.Transact(func(txn *Transaction) { t1.Insert(txn, 1, "Y", nil) })

	d3 := New(WithClientID(3))
	require.NoError(t, ApplyUpdateV1(d3, EncodeStateAsUpdateV1(d1, nil), nil))
	t3 := d3.GetText("t")
	d3.Transact(func(txn *Transaction) { t3.Insert(txn, 2, "a", nil) })
	withoutB = EncodeStateAsUpdateV1(d3, nil)
	sv := d3.StateVector()
	d3.Transact(func(txn *Transaction) { t3.Insert(txn, 3, "b", nil) })
	b := d3.store.Find(ID{Client: 3, Clock: 1})
	require.NotNil(t, b)
	b.Origin = &ID{Client: 5, Clock: 0}
	return withoutB, EncodeStateAsUpdateV1(d3, sv), EncodeStateAsUpdateV1(d3, nil), x
}

func TestUnit_SquashRuns_KeepsAnItemWhoseOriginIsNotTheRunEnd(t *testing.T) {
	withoutB, onlyB, whole, x := crossedOriginRun(t)

	oneApply := New()
	require.NoError(t, ApplyUpdateV1(oneApply, whole, nil))
	require.Equal(t, "XYab", oneApply.GetText("t").ToString())
	require.Len(t, oneApply.store.clients[3], 2, "a and b must stay two items")

	// A peer that receives a and b in separate updates never merges them.
	twoApplies := New()
	require.NoError(t, ApplyUpdateV1(twoApplies, withoutB, nil))
	require.NoError(t, ApplyUpdateV1(twoApplies, onlyB, nil))

	// "W" (2:0) is inserted after X by a client that has seen only X. It shares
	// b's origin and has a lower client id, so it is placed before b.
	d2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(d2, x, nil))
	t2 := d2.GetText("t")
	d2.Transact(func(txn *Transaction) { t2.Insert(txn, 1, "W", nil) })
	w := EncodeStateAsUpdateV1(d2, nil)
	require.NoError(t, ApplyUpdateV1(oneApply, w, nil))
	require.NoError(t, ApplyUpdateV1(twoApplies, w, nil))
	require.Equal(t, "XYaWb", twoApplies.GetText("t").ToString())
	require.Equal(t, "XYaWb", oneApply.GetText("t").ToString())
}

func TestUnit_RunGC_KeepsATombstoneWhoseOriginIsNotThePreviousEnd(t *testing.T) {
	withoutB, onlyB, _, _ := crossedOriginRun(t)
	doc := New()
	require.NoError(t, ApplyUpdateV1(doc, withoutB, nil))
	require.NoError(t, ApplyUpdateV1(doc, onlyB, nil))
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Delete(txn, 2, 2) })
	require.Equal(t, "XY", text.ToString())
	RunGC(doc)
	require.Len(t, doc.store.clients[3], 2, "the tombstones of a and b must stay two items")
	require.Equal(t, &ID{Client: 5, Clock: 0}, doc.store.clients[3][1].Origin)
}
