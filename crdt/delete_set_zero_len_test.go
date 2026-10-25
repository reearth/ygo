package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// deleteOnlyUpdateV1 encodes a V1 update with no structs and the given delete
// ranges for one client.
func deleteOnlyUpdateV1(client ClientID, ranges ...DeleteRange) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // struct groups
	enc.WriteVarUint(1) // delete-set clients
	enc.WriteVarUint(uint64(client))
	enc.WriteVarUint(uint64(len(ranges)))
	for _, r := range ranges {
		enc.WriteVarUint(r.Clock)
		enc.WriteVarUint(r.Len)
	}
	return enc.Bytes()
}

// A zero-length delete range deletes nothing, as in Yjs, wherever it points.
func TestDeleteSet_ZeroLengthRangeDeletesNothing(t *testing.T) {
	formats := []struct {
		name  string
		conv  func([]byte) ([]byte, error)
		apply func(*Doc, []byte, any) error
	}{
		{"V1", func(u []byte) ([]byte, error) { return u, nil }, ApplyUpdateV1},
		{"V2", UpdateV1ToV2, ApplyUpdateV2},
	}
	for _, f := range formats {
		t.Run(f.name, func(t *testing.T) {
			for _, clock := range []uint64{0, 1, 2, 3, 99} {
				doc := New(WithClientID(1))
				txt := doc.GetText("t")
				doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })

				u, err := f.conv(deleteOnlyUpdateV1(1, DeleteRange{Clock: clock, Len: 0}))
				require.NoError(t, err)
				require.NoError(t, f.apply(doc, u, nil))
				require.Equal(t, "abc", txt.ToString(), "zero-length range at clock %d", clock)
			}

			// Alongside a real range, only the real range deletes.
			doc := New(WithClientID(1))
			txt := doc.GetText("t")
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
			u, err := f.conv(deleteOnlyUpdateV1(1, DeleteRange{Clock: 0, Len: 0}, DeleteRange{Clock: 1, Len: 0}, DeleteRange{Clock: 2, Len: 1}))
			require.NoError(t, err)
			require.NoError(t, f.apply(doc, u, nil))
			require.Equal(t, "ab", txt.ToString())
		})
	}
}

// A zero-length range for a client the doc has not seen is not parked, so it
// cannot delete that client's content once it arrives.
func TestDeleteSet_ZeroLengthRangeIsNotParked(t *testing.T) {
	src := New(WithClientID(2))
	srcTxt := src.GetText("t")
	src.Transact(func(txn *Transaction) { srcTxt.Insert(txn, 0, "abc", nil) })

	doc := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(doc, deleteOnlyUpdateV1(2, DeleteRange{Clock: 1, Len: 0}), nil))
	require.Empty(t, doc.store.pendingDs.clients, "a zero-length range must not be parked")

	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(src, nil), nil))
	require.Equal(t, "abc", doc.GetText("t").ToString())
}
