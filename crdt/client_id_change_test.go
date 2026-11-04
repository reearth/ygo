package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// A remote update that moves the document's own client clock gives the
// document a new client ID, as Yjs does: a peer's GC struct over the room's
// own client cannot make the room's next local write continue that clock, so
// the room's state still loads.
func TestUnit_RemoteChangeToOwnClockTakesANewClientID(t *testing.T) {
	for _, end := range []uint64{maxSafeInteger, maxSafeInteger + 1} {
		room := New(WithClientID(5))
		txt := room.GetText("t")
		room.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "x", nil) })
		// A peer's GC struct over client 5's clocks [0, end).
		enc := encoding.NewEncoder()
		for _, w := range []uint64{1, 1, 5, 0, 0, end, 0} {
			enc.WriteVarUint(w)
		}
		require.NoError(t, ApplyUpdateV1(room, enc.Bytes(), nil))
		room.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ab", nil) })
		// V1 only: the V2 decoder reads a struct length of at most 2^31-1.
		reload := New()
		require.NoError(t, ApplyUpdateV1(reload, EncodeStateAsUpdateV1(room, nil), nil), "end %d", end)
		require.Equal(t, "abx", reload.GetText("t").ToString())
		require.Equal(t, room.StateVector(), reload.StateVector())
		require.NotEqual(t, ClientID(5), room.ClientID(), "end %d", end)
	}

	// A local transaction, and a remote one that leaves the own clock alone,
	// keep the ID.
	doc, other := New(WithClientID(7)), New(WithClientID(8))
	dt, ot := doc.GetText("t"), other.GetText("t")
	doc.Transact(func(txn *Transaction) { dt.Insert(txn, 0, "a", nil) })
	other.Transact(func(txn *Transaction) { ot.Insert(txn, 0, "b", nil) })
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(other, nil), nil))
	require.Equal(t, ClientID(7), doc.ClientID())
}
