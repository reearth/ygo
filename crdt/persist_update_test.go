package crdt_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

func TestUnit_Doc_OnPersistUpdate(t *testing.T) {
	parent := crdt.New(crdt.WithClientID(1))
	root := parent.GetMap("m")
	parent.Transact(func(txn *crdt.Transaction) { root.Set(txn, "nested", crdt.NewMapPrelim()) })
	parentUpdate := crdt.EncodeStateAsUpdateV1(parent, nil)
	author := crdt.New(crdt.WithClientID(2))
	require.NoError(t, crdt.ApplyUpdateV1(author, parentUpdate, nil))
	nested, ok := author.GetMap("m").Get("nested")
	require.True(t, ok)
	before := author.StateVector()
	author.Transact(func(txn *crdt.Transaction) { nested.(*crdt.YMap).Set(txn, "key", "value") })
	parked := crdt.EncodeStateAsUpdateV1(author, before)

	room := crdt.New(crdt.WithClientID(3))
	var stored [][]byte
	var updates int
	origin := &struct{ name string }{"author"}
	unsubscribe := room.OnPersistUpdate(func(update []byte, got any) {
		require.Same(t, origin, got)
		stored = append(stored, append([]byte(nil), update...))
	})
	room.OnUpdate(func([]byte, any) { updates++ })
	require.NoError(t, crdt.ApplyUpdateV1(room, parked, origin))
	require.Equal(t, 0, updates, "parking alone is not a Yjs update event")
	require.Len(t, stored, 1)
	require.Equal(t, 1, room.PendingStats().Items)

	require.NoError(t, crdt.ApplyUpdateV1(room, parked, origin))
	require.Len(t, stored, 1, "re-sending parked state must not persist it again")

	replay := crdt.New(crdt.WithClientID(4))
	require.NoError(t, crdt.ApplyUpdateV1(replay, stored[0], nil))
	require.Equal(t, 1, replay.PendingStats().Items)
	require.NoError(t, crdt.ApplyUpdateV1(replay, parentUpdate, nil))
	v, ok := replay.GetMap("m").Get("nested")
	require.True(t, ok)
	value, ok := v.(*crdt.YMap).Get("key")
	require.True(t, ok)
	require.Equal(t, "value", value)
	require.Zero(t, replay.PendingStats().Items)

	unsubscribe()
	require.NoError(t, crdt.ApplyUpdateV1(room, parentUpdate, nil))
	require.Len(t, stored, 1, "unsubscribing detaches the persistence observer")
}
