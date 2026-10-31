package crdt

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A map entry that arrives with deleted content and wins its key deletes the
// previous winner first, so the winner's client leads the delete set, as in
// Yjs's Item.integrate. The prior state and update are Yjs's: client 11 sets
// k, client 12 sets and then deletes k.
func TestUnit_YMap_ArrivedDeletionWinnerDeletedFirst(t *testing.T) {
	for _, tc := range []struct {
		name          string
		apply         func(*Doc, []byte, any) error
		prior, update string
	}{
		{"v1", ApplyUpdateV1, "01010b002801016d016b017d0100", "01010c00a10b0001020c0100010b010001"},
		{"v2", ApplyUpdateV2, "0000010b0000012805026d6b410001010001010101007d0100", "0000020c0b0100000181010000000101010100020c0100000b010000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(2)
			require.NoError(t, tc.apply(doc, mustHex(t, tc.prior), nil))
			var order []ClientID
			doc.OnAfterTransaction(func(txn *Transaction) { order = txn.deleteSet.orderedClients() })
			require.NoError(t, tc.apply(doc, mustHex(t, tc.update), nil))
			assert.Equal(t, []ClientID{11, 12}, order)
		})
	}
}

// A move parked on a run that arrives with deleted content splits the run as
// it integrates. Every part stays deleted, whether the move or the run arrives
// first, also when the update's delete set leaves the run out.
func TestUnit_YArray_ArrivedDeletionSplitByPendingMove(t *testing.T) {
	m := newTestDoc(5)
	ma := m.GetArray("a")
	m.Transact(func(txn *Transaction) { ma.Insert(txn, 0, []any{9}) })
	a := newTestDoc(10)
	aa := a.GetArray("a")
	require.NoError(t, ApplyUpdateV1(a, EncodeStateAsUpdateV1(m, nil), nil))
	a.Transact(func(txn *Transaction) { aa.Insert(txn, 1, []any{1, 2, 3}) })
	require.NoError(t, ApplyUpdateV1(m, EncodeStateAsUpdateV1(a, nil), nil))
	m.Transact(func(txn *Transaction) { ma.Move(txn, 2, 0) })
	move := EncodeStateAsUpdateV1(m, StateVector{10: 3})
	a.Transact(func(txn *Transaction) { aa.Delete(txn, 1, 3) })
	run := EncodeStateAsUpdateV1(a, StateVector{5: 1})
	full := EncodeStateAsUpdateV1(a, nil)

	// The run's delete set (client 10, clocks 0-2) is the last bytes of both updates.
	ds := mustHex(t, "010a010003")
	require.True(t, bytes.HasSuffix(run, ds))
	require.True(t, bytes.HasSuffix(full, ds))
	noDS := func(u []byte) []byte { return append(bytes.Clone(u[:len(u)-len(ds)]), 0) }

	for _, tc := range []struct {
		name      string
		run, full []byte
	}{{"covered", run, full}, {"empty_delete_set", noDS(run), noDS(full)}} {
		t.Run(tc.name, func(t *testing.T) {
			moveFirst := newTestDoc(2)
			require.NoError(t, ApplyUpdateV1(moveFirst, move, nil))
			var txnDS DeleteSet
			moveFirst.OnAfterTransaction(func(txn *Transaction) { txnDS = cloneDeleteSet(txn.deleteSet) })
			require.NoError(t, ApplyUpdateV1(moveFirst, tc.run, nil))
			assert.Equal(t, []DeleteRange{{Clock: 0, Len: 3}}, txnDS.clients[10])
			runFirst := newTestDoc(3)
			require.NoError(t, ApplyUpdateV1(runFirst, tc.full, nil))
			require.NoError(t, ApplyUpdateV1(runFirst, move, nil))
			for _, d := range []*Doc{moveFirst, runFirst} {
				for _, it := range d.store.clients[10] {
					assert.True(t, it.Deleted, "item %v", it.ID)
				}
			}
			assert.Equal(t, EncodeStateAsUpdateV1(runFirst, nil), EncodeStateAsUpdateV1(moveFirst, nil))
		})
	}
}
