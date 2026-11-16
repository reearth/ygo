package websocket_test

import (
	"testing"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// A client whose tail parks in the room, waiting for another client's edit
// the room has not received, reconnects ten times and sends the tail again
// each time, as a client re-sends what the room did not acknowledge. The room
// parks the tail once: its pending count stays at the tail's structs, and with
// the room's pending cap at exactly that many, no resend is refused.
func TestInteg_ResentParkedTailParksOnce(t *testing.T) {
	const tail = 5
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) { srv.MaxPendingItems = tail })

	// Client 9 writes "ab"; the room never gets it. Client 7 has it and types
	// after it, one keystroke per update.
	other := crdt.New(crdt.WithClientID(9))
	other.Transact(func(txn *crdt.Transaction) { txn.GetText("t").Insert(txn, 0, "ab", nil) })
	writer := crdt.New(crdt.WithClientID(7))
	require.NoError(t, crdt.ApplyUpdateV1(writer, crdt.EncodeStateAsUpdateV1(other, nil), nil))
	before := writer.StateVector()
	keystrokes := make([][]byte, 0, tail)
	for i := range tail {
		sv := writer.StateVector()
		writer.Transact(func(txn *crdt.Transaction) { txn.GetText("t").Insert(txn, 2+i, "x", nil) })
		keystrokes = append(keystrokes, crdt.EncodeStateAsUpdateV1(writer, sv))
	}
	// What the writer sends again on a reconnect: everything after "ab".
	resent := crdt.EncodeStateAsUpdateV1(writer, before)

	// A watcher keeps the room loaded across the writer's reconnects.
	watcher := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))
	defer watcher.Close()
	room := srv.GetDoc("room")
	first := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(99)))
	for _, k := range keystrokes {
		hpSend(t, first, "room", tagSync, ygsync.EncodeUpdate(k))
	}
	require.Equal(t, []uint64{1, 1, 1, 1, 1}, syncStatuses(t, first, "room"))
	require.Equal(t, tail, room.PendingStats().Items, "the keystrokes park")
	_ = first.Close()
	for i := range 10 {
		conn := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(crdt.ClientID(100+i))))
		hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(resent))
		require.Equal(t, []uint64{1}, syncStatuses(t, conn, "room"), "resend %d", i+1)
		require.Equal(t, tail, room.PendingStats().Items, "after resend %d", i+1)
		require.NoError(t, conn.WriteMessage(gws.CloseMessage, gws.FormatCloseMessage(gws.CloseNormalClosure, "")))
		_ = conn.Close()
	}

	// Client 9's edit arrives: the tail integrates once.
	conn := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(200)))
	defer conn.Close()
	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(crdt.EncodeStateAsUpdateV1(other, nil)))
	require.Equal(t, []uint64{1}, syncStatuses(t, conn, "room"))
	require.Equal(t, "abxxxxx", room.GetText("t").ToString())
	require.Zero(t, room.PendingStats().Items)
}
