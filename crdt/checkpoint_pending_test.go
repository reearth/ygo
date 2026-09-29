package crdt

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_Checkpoint_ResolvesSameUpdateBeforePendingLimit(t *testing.T) {
	for _, ids := range [][2]uint64{{1, 2}, {2, 1}} {
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("v%d/%d-%d", version, ids[0], ids[1]), func(t *testing.T) {
				parent := newTestDoc(ids[0])
				defer parent.Destroy()
				array := parent.GetArray("items")
				parent.Transact(func(txn *Transaction) { array.Insert(txn, 0, []any{"seed"}) })
				writer := newTestDoc(ids[1])
				defer writer.Destroy()
				require.NoError(t, ApplyUpdateV1(writer, EncodeStateAsUpdateV1(parent, nil), nil))
				edited := writer.GetArray("items")
				for i := 0; i < 10; i++ {
					writer.Transact(func(txn *Transaction) { edited.Insert(txn, i+1, []any{i}) })
				}
				receiver := New(WithMaxPendingItems(3))
				defer receiver.Destroy()
				var err error
				if version == 1 {
					err = ApplyUpdateV1(receiver, EncodeStateAsUpdateV1(writer, nil), nil)
				} else {
					err = ApplyUpdateV2(receiver, EncodeStateAsUpdateV2(writer, nil), nil)
				}
				require.NoError(t, err)
				require.Zero(t, receiver.PendingStats().Items)
				require.Equal(t, edited.ToSlice(), receiver.GetArray("items").ToSlice())
				// A truly incomplete update must still respect the configured cap.
				missing := New(WithMaxPendingItems(3))
				defer missing.Destroy()
				if version == 1 {
					err = ApplyUpdateV1(missing, EncodeStateAsUpdateV1(writer, parent.StateVector()), nil)
				} else {
					err = ApplyUpdateV2(missing, EncodeStateAsUpdateV2(writer, parent.StateVector()), nil)
				}
				require.ErrorIs(t, err, ErrInvalidUpdate)
				require.LessOrEqual(t, missing.PendingStats().Items, 3)
			})
		}
	}
}
