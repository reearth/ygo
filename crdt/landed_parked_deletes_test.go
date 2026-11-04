package crdt

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An update whose delete set parks a range and whose drain then fails lands
// that range, so a peer given what landed parks what the room parks.
func TestUnit_ApplyUpdateV1Landed_CarriesTheDeletesItParkedWhenTheDrainFails(t *testing.T) {
	w := New(WithClientID(13))
	wt := w.GetText("t")
	var ups [][]byte
	w.OnUpdate(func(u []byte, _ any) { ups = append(ups, append([]byte(nil), u...)) })
	for i, s := range []string{"a", "b"} {
		w.Transact(func(txn *Transaction) { wt.Insert(txn, i, s, nil) })
	}
	// "a" and a deletion of client 77's clock 5, which nobody holds.
	update, err := MergeUpdatesV1(ups[0], deleteSetUpdate(map[ClientID][]DeleteRange{77: {{Clock: 5, Len: 1}}}))
	require.NoError(t, err)

	newRoom := func() *Doc {
		d := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
		require.NoError(t, ApplyUpdateV1(d, ups[1], nil)) // "b" parks behind 13:0
		return d
	}
	room := newRoom()
	room.store.pending.items[0].Content = nil // the drain panics on "b"
	landed, err := ApplyUpdateV1Landed(room, update, nil)
	require.ErrorIs(t, err, ErrInvalidUpdate)
	require.NotNil(t, landed)
	require.Equal(t, 1, room.PendingStats().DeleteRanges, "the room parks the deletion")

	ids, err := ContentIDsFromUpdateV1(landed)
	require.NoError(t, err)
	assert.Equal(t, []IDRange{{Clock: 5, Len: 1}}, ids.Deletes.Ranges(77), "what landed carries the parked deletion")

	peer := New(WithClientID(2), WithMaxPendingItems(math.MaxInt))
	require.NoError(t, ApplyUpdateV1(peer, landed, nil))
	assert.Equal(t, room.GetText("t").ToString(), peer.GetText("t").ToString())
	assert.Equal(t, room.PendingStats().DeleteRanges, peer.PendingStats().DeleteRanges, "the peer parks what the room parks")
}
