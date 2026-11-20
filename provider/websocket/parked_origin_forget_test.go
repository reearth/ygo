package websocket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// A peer that parked a delete range and left is forgotten by the room's
// record of who parked what (crdt.Doc.ForgetParkedOrigin), so the record
// keeps neither the peer nor its connection: a discard of what it parked
// names no sender for it, while a peer still connected is named.
func TestInteg_DiscardRefusedDrain_APeerThatLeftIsNotKeptAsASender(t *testing.T) {
	left := make(chan struct{}, 1)
	loaded := make(chan *crdt.Doc, 1)
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.MaxPendingDeleteRanges = 4096
		srv.RoomIdleTimeout = time.Hour // the room stays loaded once the first peer has left
		srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
			doc.SetDiscardRefusedDrain(true)
			doc.SetApplyBudget(func(any) crdt.ApplyBudget {
				return crdt.ApplyBudget{Decoded: -1, Structs: -1, Bytes: -1, Split: -1, MayParkDeletes: func(crdt.ClientID) bool { return true }}
			})
			loaded <- doc
			return nil
		}
		srv.OnLastPeer = func(context.Context, string) { left <- struct{}{} }
	})

	gone := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(1000)))
	room := <-loaded
	hpSend(t, gone, "room", tagSync, ygsync.EncodeUpdate(missingDeleteRanges(1000, 1)))
	require.Equal(t, []uint64{1}, syncStatuses(t, gone, "room"))
	require.NoError(t, gone.Close())
	<-left

	stays := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(2000)))
	hpSend(t, stays, "room", tagSync, ygsync.EncodeUpdate(missingDeleteRanges(2000, 1)))
	require.Equal(t, []uint64{1}, syncStatuses(t, stays, "room"))
	require.Equal(t, 2, room.PendingStats().DeleteRanges)

	// A write of both clients' first clocks lands, and its drain, which
	// applies both parked deletions, is refused and discarded.
	text := room.GetText("t")
	room.SetApplyCheck(func(_ any, view *crdt.ApplyView) error {
		if len(view.TextString(text)) < 2 {
			return errors.New("the drain deletes what the write wrote")
		}
		return nil
	})
	fill := make([][]byte, 0, 2)
	for _, client := range []crdt.ClientID{1000, 2000} {
		writer := crdt.New(crdt.WithClientID(client))
		wtext := writer.GetText("t")
		writer.Transact(func(txn *crdt.Transaction) { wtext.Insert(txn, 0, "x", nil) })
		fill = append(fill, crdt.EncodeStateAsUpdateV1(writer, nil))
	}
	update, err := crdt.MergeUpdatesV1(fill...)
	require.NoError(t, err)
	err = crdt.ApplyUpdateV1(room, update, "server")
	var discarded *crdt.DrainDiscardedError
	require.ErrorAs(t, err, &discarded)
	assert.Equal(t, 2, discarded.Ranges)
	require.Len(t, discarded.Senders, 1, "only the peer still connected is named; the one that left is kept by nothing")
	assert.NotEqual(t, "server", discarded.Senders[0])
}
