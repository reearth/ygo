package websocket_test

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"
)

// A peer holds what the room holds after a partly applied update drains the
// "b" it unblocked.
func TestInteg_PartlyAppliedUpdate_PeerGetsWhatTheRoomDrained(t *testing.T) {
	w := crdt.New(crdt.WithClientID(13))
	wt := w.GetText("t")
	var ups [][]byte
	w.OnUpdate(func(u []byte, _ any) { ups = append(ups, append([]byte(nil), u...)) })
	for i, s := range []string{"a", "b", "c"} {
		w.Transact(func(txn *crdt.Transaction) { wt.Insert(txn, i, s, nil) })
	}
	both, err := crdt.MergeUpdatesV1(ups[0], ups[2])
	require.NoError(t, err)

	for _, c := range []struct {
		name string
		bad  []byte
	}{
		{"delete set cut", ups[0][:len(ups[0])-1]},
		{"struct cut", both[:len(both)-3]},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, ts := hocuspocusServer(t, nil)
			conn := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(7)))
			peerDoc := crdt.New(crdt.WithClientID(9))
			peer := hpDial(t, ts, "room", false, peerDoc)

			hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(ups[1]))
			require.Equal(t, []uint64{1}, syncStatuses(t, conn, "room"), "\"b\" parks and is answered 1")
			hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(c.bad))
			require.Equal(t, []uint64{0}, syncStatuses(t, conn, "room"), "the cut update is answered 0")
			watcherSyncBytes(t, peer, peerDoc)

			room := srv.GetDoc("room")
			require.Equal(t, "ab", room.GetText("t").ToString(), "the room drains \"b\" behind the \"a\" that landed")
			require.Equal(t, room.GetText("t").ToString(), peerDoc.GetText("t").ToString())
			require.True(t, maps.Equal(room.StateVector(), peerDoc.StateVector()),
				"room sv %v, peer sv %v", room.StateVector(), peerDoc.StateVector())
			require.Equal(t, room.PendingStats().Items, peerDoc.PendingStats().Items)
		})
	}
}
