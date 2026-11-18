package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// parkedEdits is two updates of client 2 to text "t": first inserts "ab", and
// second, encoded from the state after first, inserts "cd" after it and
// deletes "a". A document holding neither parks second's struct, waiting
// for "ab", and its deletion of "a".
func parkedEdits(t *testing.T) (first, second []byte) {
	t.Helper()
	src := New(WithClientID(2))
	text := src.GetText("t")
	src.Transact(func(txn *Transaction) { text.Insert(txn, 0, "ab", nil) })
	first = EncodeStateAsUpdateV1(src, nil)
	sv := src.StateVector()
	src.Transact(func(txn *Transaction) {
		text.Insert(txn, 2, "cd", nil)
		text.Delete(txn, 0, 1)
	})
	second = EncodeStateAsUpdateV1(src, sv)
	return first, second
}

func applyAs(t *testing.T, doc *Doc, v2 bool, update []byte) {
	t.Helper()
	if !v2 {
		require.NoError(t, ApplyUpdateV1(doc, update, nil))
		return
	}
	converted, err := UpdateV1ToV2(update)
	require.NoError(t, err)
	require.NoError(t, ApplyUpdateV2(doc, converted, nil))
}

// A logged apply that parks a struct and a deletion logs them: the log's base
// plus its updates holds the document's whole state, what it holds parked
// included, after the commit. OnUpdate's update still carries only what the
// transaction integrated, as in Yjs.
func TestUnit_UpdateLog_ReplayHoldsWhatALoggedApplyParked(t *testing.T) {
	_, second := parkedEdits(t)
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1))
		log := room.OpenUpdateLog()
		var observed []byte
		room.OnUpdate(func(update []byte, _ any) { observed = update })
		applyAs(t, room, v2, second)
		stats := room.PendingStats()
		require.Equal(t, 1, stats.Items, "v2=%v", v2)
		require.Equal(t, 1, stats.DeleteRanges, "v2=%v", v2)

		base, updates := log.Drain()
		require.Len(t, updates, 1, "v2=%v", v2)
		replica := replay(t, base, updates)
		require.Equal(t, EncodeStateAsUpdateV1(room, nil), EncodeStateAsUpdateV1(replica, nil), "v2=%v", v2)
		require.Equal(t, stats, replica.PendingStats(), "v2=%v", v2)

		bare := New(WithClientID(3))
		require.NoError(t, ApplyUpdateV1(bare, observed, nil))
		require.Zero(t, bare.PendingStats().Items, "v2=%v: OnUpdate's update carries a parked struct", v2)
		require.Zero(t, bare.PendingStats().DeleteRanges, "v2=%v: OnUpdate's update carries a parked deletion", v2)
	}
}

// A room rebuilt from its log, as one is from stored updates after a restart,
// converges with the room when the predecessor its log's apply parked behind
// arrives later: both hold "bcd". Rebuilt from updates without the parked
// struct and deletion, it would hold "ab".
func TestUnit_UpdateLog_ARebuiltRoomConvergesWhenTheMissingPredecessorArrives(t *testing.T) {
	first, second := parkedEdits(t)
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1))
		log := room.OpenUpdateLog()
		applyAs(t, room, v2, second)
		base, updates := log.Drain()
		rebuilt := replay(t, base, updates)

		applyAs(t, room, v2, first)
		applyAs(t, rebuilt, v2, first)
		require.Equal(t, "bcd", room.GetText("t").ToString(), "v2=%v", v2)
		require.Equal(t, room.GetText("t").ToString(), rebuilt.GetText("t").ToString(), "v2=%v", v2)
		require.Equal(t, EncodeStateAsUpdateV1(room, nil), EncodeStateAsUpdateV1(rebuilt, nil), "v2=%v", v2)
		require.Zero(t, rebuilt.PendingStats().Items, "v2=%v", v2)
		require.Zero(t, rebuilt.PendingStats().DeleteRanges, "v2=%v", v2)

		// The commit that integrated the parked struct is logged too, and a
		// replica following the log the whole way converges as well. It took
		// "b" and "cd" in one update, so it may hold them as one struct where
		// the room holds two: compare content, clocks and deletions.
		_, later := log.Drain()
		follower := replay(t, base, append(updates, later...))
		require.Equal(t, room.GetText("t").ToString(), follower.GetText("t").ToString(), "v2=%v", v2)
		require.Equal(t, room.StateVector(), follower.StateVector(), "v2=%v", v2)
		require.Equal(t, buildDeleteSet(room.store).clients, buildDeleteSet(follower.store).clients, "v2=%v", v2)
		require.Zero(t, follower.PendingStats().Items, "v2=%v", v2)
	}
}

// A commit logs the very slice its OnUpdate observers get, whether it parks
// or not: what it parked is beside it, in Parked.
func TestUnit_UpdateLog_ACommitLogsTheObservedSlice(t *testing.T) {
	_, first := parkedEdits(t)
	room := New(WithClientID(1))
	log := room.OpenUpdateLog()
	var observed []byte
	room.OnUpdate(func(update []byte, _ any) { observed = update })
	require.NoError(t, ApplyUpdateV1(room, first, nil))
	_, updates := log.Drain()
	require.Len(t, updates, 1)
	require.NotEmpty(t, observed)
	require.Same(t, &observed[0], &updates[0].Update[0])
}
