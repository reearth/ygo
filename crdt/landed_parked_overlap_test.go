package crdt

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// A struct that parks over clocks the room holds does not hide the deletions
// the apply made in those clocks: a failed apply's update carries them, and a
// peer given it deletes what the room deleted.
func TestUnit_ApplyUpdateV1Landed_KeepsTheDeletionsUnderAParkedStruct(t *testing.T) {
	writer := New(WithClientID(5))
	wt := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { wt.Insert(txn, 0, "0123456789", nil) })
	state := EncodeStateAsUpdateV1(writer, nil)

	w := New(WithClientID(13))
	ut := w.GetText("u")
	var ups [][]byte
	w.OnUpdate(func(u []byte, _ any) { ups = append(ups, append([]byte(nil), u...)) })
	for i, s := range []string{"a", "b"} {
		w.Transact(func(txn *Transaction) { ut.Insert(txn, i, s, nil) })
	}

	// Client 13's "a", which lets the room retry its parked "b"; client 5's
	// "ABCDEFG" at clock 5, over clocks the room holds, with a right origin
	// nobody holds, so it parks; and a deletion of client 5's clocks 6 to 9.
	enc := encoding.NewEncoder()
	enc.WriteVarUint(2)
	enc.WriteVarUint(1)
	enc.WriteVarUint(13)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireString)
	enc.WriteUint8(1)
	enc.WriteVarString("u")
	enc.WriteVarString("a")
	enc.WriteVarUint(1)
	enc.WriteVarUint(5)
	enc.WriteVarUint(5)
	enc.WriteUint8(wireString | flagHasRightOrigin)
	enc.WriteVarUint(77)
	enc.WriteVarUint(0)
	enc.WriteVarString("ABCDEFG")
	enc.WriteVarUint(1)
	enc.WriteVarUint(5)
	enc.WriteVarUint(1)
	enc.WriteVarUint(6)
	enc.WriteVarUint(4)
	update := enc.Bytes()

	room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
	require.NoError(t, ApplyUpdateV1(room, state, nil))
	require.NoError(t, ApplyUpdateV1(room, ups[1], nil)) // "b" parks behind 13:0
	b := room.store.pending.items[0]
	b.Content = drainPanicContent{b.Content} // the drain panics on "b", so the apply fails
	landed, err := ApplyUpdateV1Landed(room, update, nil)
	require.Error(t, err)
	require.NotNil(t, landed)
	require.Equal(t, "012345", room.GetText("t").ToString(), "the room deleted clocks 6 to 9")

	ids, err := ContentIDsFromUpdateV1(landed)
	require.NoError(t, err)
	assert.Equal(t, []IDRange{{Clock: 6, Len: 4}}, ids.Deletes.Ranges(5), "what the apply deleted travels")

	peer := New(WithClientID(2), WithMaxPendingItems(math.MaxInt))
	require.NoError(t, ApplyUpdateV1(peer, state, nil))
	require.NoError(t, ApplyUpdateV1(peer, landed, nil))
	assert.Equal(t, room.GetText("t").ToString(), peer.GetText("t").ToString(), "the peer deletes what the room deleted")
}

// drainPanicContent panics when an integration asks whether it counts, and
// answers every other question as the content it wraps.
type drainPanicContent struct{ Content }

func (drainPanicContent) IsCountable() bool { panic("parked content panicked") }
