package crdt

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// landedParking returns an update in which client sets n keys on a map that
// another client created and the receiver never got, so every entry parks.
func landedParking(t *testing.T, client uint64, n int) []byte {
	t.Helper()
	author := newTestDoc(client + 1000)
	am := author.GetMap("m")
	author.Transact(func(txn *Transaction) { am.Set(txn, "nested", NewMapPrelim()) })
	editor := newTestDoc(client)
	require.NoError(t, ApplyUpdateV1(editor, EncodeStateAsUpdateV1(author, nil), nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested := v.(*YMap)
	editor.Transact(func(txn *Transaction) {
		for i := range n {
			nested.Set(txn, "k"+strconv.Itoa(i), i)
		}
	})
	return EncodeStateAsUpdateV1(editor, author.StateVector())
}

func TestUnit_ApplyUpdateV1Landed(t *testing.T) {
	// The room holds 50 deletions and 4 parked entries, which an encoding of
	// its state would carry, and the writer's key w.a (client 3, clock 100).
	writer := newTestDoc(3)
	wt := writer.GetText("t")
	wm := writer.GetMap("w")
	writer.Transact(func(txn *Transaction) { wt.Insert(txn, 0, strings.Repeat("x", 100), nil) })
	writer.Transact(func(txn *Transaction) { wt.Delete(txn, 0, 50) })
	writer.Transact(func(txn *Transaction) { wm.Set(txn, "a", 1) })
	written := EncodeStateAsUpdateV1(writer, nil)
	newRoom := func() *Doc {
		room := New(WithClientID(100), WithMaxPendingItems(10))
		require.NoError(t, ApplyUpdateV1(room, written, nil))
		require.NoError(t, ApplyUpdateV1(room, landedParking(t, 2, 4), nil))
		require.Equal(t, 4, room.PendingStats().Items)
		return room
	}

	// The editor inserts "abc" and overwrites w.a, which integrate (the
	// overwrite deletes the writer's entry), then sets seven entries on a map
	// the room never received, which overflow the six pending slots left. Its
	// own deletion of a character travels in the update's delete set, which
	// the apply never reaches.
	editor := newTestDoc(7)
	require.NoError(t, ApplyUpdateV1(editor, written, nil))
	author := newTestDoc(1)
	am := author.GetMap("m")
	author.Transact(func(txn *Transaction) { am.Set(txn, "nested", NewMapPrelim()) })
	require.NoError(t, ApplyUpdateV1(editor, EncodeStateAsUpdateV1(author, nil), nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested := v.(*YMap)
	et, em := editor.GetText("t"), editor.GetMap("w")
	before := editor.StateVector() // includes the author's map, so it is not sent
	editor.Transact(func(txn *Transaction) {
		et.Insert(txn, 0, "abc", nil)
		et.Delete(txn, 3, 1)
		em.Set(txn, "a", 2)
		for i := range 7 {
			nested.Set(txn, "e"+strconv.Itoa(i), i)
		}
	})
	overflowing := EncodeStateAsUpdateV1(editor, before)

	room := newRoom()
	landed, err := ApplyUpdateV1Landed(room, overflowing, nil)
	require.ErrorIs(t, err, ErrInvalidUpdate)
	require.Equal(t, "abc"+strings.Repeat("x", 50), room.GetText("t").ToString(), "the insertion integrated")
	require.NotNil(t, landed)

	ids, err := ContentIDsFromUpdateV1(landed)
	require.NoError(t, err)
	assert.Equal(t, []ClientID{7}, ids.Inserts.Clients(), "only structs this apply integrated")
	assert.Equal(t, []IDRange{{Clock: 0, Len: 4}}, ids.Inserts.Ranges(7), `"abc" and w.a`)
	assert.Equal(t, []ClientID{3}, ids.Deletes.Clients(), "only the deletion this apply made")
	assert.Equal(t, []IDRange{{Clock: 100, Len: 1}}, ids.Deletes.Ranges(3), "the overwritten w.a")

	// A peer that held what the room held before ends where the room did.
	peer := newRoom()
	require.NoError(t, ApplyUpdateV1(peer, landed, nil))
	assert.Equal(t, room.GetText("t").ToString(), peer.GetText("t").ToString())

	t.Run("an update that does not decode, and one that applies, land nil", func(t *testing.T) {
		landed, err := ApplyUpdateV1Landed(newRoom(), EncodeStateAsUpdateV1(editor, nil)[:0], nil)
		require.Error(t, err, "an empty byte slice does not decode")
		assert.Nil(t, landed, "nothing integrated")

		doc := newTestDoc(9)
		landed, err = ApplyUpdateV1Landed(doc, EncodeStateAsUpdateV1(newRoom(), nil), nil)
		require.NoError(t, err)
		assert.Nil(t, landed)
	})

	t.Run("a failure that integrates nothing lands nil", func(t *testing.T) {
		room := newRoom()
		landed, err := ApplyUpdateV1Landed(room, landedParking(t, 5, 7), nil)
		require.ErrorIs(t, err, ErrInvalidUpdate)
		assert.Nil(t, landed)
	})
}
