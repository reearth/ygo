package websocket_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"
)

// An update that integrates "ok", parks "xy" (it follows a "Z" the room lacks)
// and then fails on a delete set cut short must reach the other peers with
// "xy" parked. When a third peer later sends the "Z", the room drains "xy";
// a watcher that only followed the relay must show the same text.
func TestInteg_PartlyAppliedUpdate_RelaysWhatItParked(t *testing.T) {
	srv, ts := hocuspocusServer(t, nil)
	z := crdt.New(crdt.WithClientID(9))
	zt := z.GetText("t")
	z.Transact(func(txn *crdt.Transaction) { zt.Insert(txn, 0, "Z", nil) })
	filler := crdt.EncodeStateAsUpdateV1(z, nil)

	parker := crdt.New(crdt.WithClientID(5))
	require.NoError(t, crdt.ApplyUpdateV1(parker, filler, nil))
	pt := parker.GetText("t")
	parker.Transact(func(txn *crdt.Transaction) { pt.Insert(txn, 1, "xy", nil) })
	ok := crdt.New(crdt.WithClientID(6))
	ot := ok.GetText("t")
	ok.Transact(func(txn *crdt.Transaction) { ot.Insert(txn, 0, "ok", nil) })
	valid, err := crdt.MergeUpdatesV1(crdt.EncodeStateAsUpdateV1(parker, z.StateVector()), crdt.EncodeStateAsUpdateV1(ok, nil))
	require.NoError(t, err)
	require.Equal(t, byte(0), valid[len(valid)-1], "the update ends in an empty delete set")
	// One client (5) with one range, and no clock: the delete set ends early.
	failing := append(valid[:len(valid)-1:len(valid)-1], 1, 5, 1)

	editor := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(7)))
	watcherDoc := crdt.New(crdt.WithClientID(8))
	watcher := hpDial(t, ts, "room", false, watcherDoc)
	third := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(10)))

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(failing))
	require.Equal(t, []uint64{0}, syncStatuses(t, editor, "room"))
	room := srv.GetDoc("room")
	require.Equal(t, "ok", room.GetText("t").ToString())
	require.Equal(t, 1, room.PendingStats().Items, "the room parks \"xy\"")
	watcherSyncBytes(t, watcher, watcherDoc)
	require.Equal(t, room.PendingStats(), watcherDoc.PendingStats(), "the watcher parks what the room parks")

	hpSend(t, third, "room", tagSync, ygsync.EncodeUpdate(filler))
	require.Equal(t, []uint64{1}, syncStatuses(t, third, "room"))
	watcherSyncBytes(t, watcher, watcherDoc)
	require.Len(t, room.GetText("t").ToString(), 5, "the room drained \"xy\"")
	require.Equal(t, room.GetText("t").ToString(), watcherDoc.GetText("t").ToString())
	require.Equal(t, crdt.EncodeStateAsUpdateV1(room, nil), crdt.EncodeStateAsUpdateV1(watcherDoc, nil))
}
