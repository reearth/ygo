package crdt

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// The delete set the parked-range check reads and the apply then uses gives
// the document a fresh decode gives: a cap that makes the check read it and
// one that does not leave the same state, in V1 and V2.
func TestUnit_ApplyUpdate_TheDeleteSetTheCapReadIsTheOneApplied(t *testing.T) {
	// 400 characters with 120 deleted, so the state carries 120 delete
	// ranges of one client.
	src := New(WithClientID(1), WithGC(false))
	text := src.GetText("t")
	src.Transact(func(txn *Transaction) {
		for i := range 400 {
			text.Insert(txn, 0, strconv.Itoa(i%10), nil)
		}
	})
	for i := range 120 {
		src.Transact(func(txn *Transaction) { text.Delete(txn, i*2, 1) })
	}
	for _, f := range []struct {
		name   string
		v2     bool
		encode func(*Doc, StateVector) []byte
		apply  func(*Doc, []byte, any) error
	}{{"V1", false, EncodeStateAsUpdateV1, ApplyUpdateV1}, {"V2", true, EncodeStateAsUpdateV2, ApplyUpdateV2}} {
		t.Run(f.name, func(t *testing.T) {
			update := f.encode(src, nil)

			// A cap of 50 makes the check read the delete set whole.
			probe := New(WithClientID(9), WithMaxPendingItems(50))
			b := newPendingBudget(probe, probe.store.StateVector(), update, f.v2)
			require.NoError(t, b.knowSuperseded())
			require.NoError(t, checkParkedDeletes(probe, &b))
			require.True(t, b.deleteSetRead, "the check read the delete set")

			loose := New(WithClientID(9))
			require.NoError(t, f.apply(loose, update, nil))
			tight := New(WithClientID(9), WithMaxPendingItems(50))
			require.NoError(t, f.apply(tight, update, nil))
			require.Equal(t, loose.GetText("t").ToString(), tight.GetText("t").ToString())
			require.Equal(t, loose.PendingStats(), tight.PendingStats())
			require.Equal(t, EncodeStateAsUpdateV1(loose, nil), EncodeStateAsUpdateV1(tight, nil))
		})
	}
}
