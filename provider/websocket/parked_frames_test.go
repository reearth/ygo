package websocket_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// Tests for Server.RelayIntegratedOnly and Server.AnswerParkedNotApplied: a
// peer's frame whose structs park in the room is neither relayed to the other
// peers nor answered applied until the room lands it.

// gappedEdits returns two updates of one writer, client 7: the first inserts
// "ab", the second "cd" after it, which a room lacking the first parks.
func gappedEdits() (first, second []byte) {
	writer := crdt.New(crdt.WithClientID(7))
	first = textUpdate(writer, "ab")
	before := writer.StateVector()
	txt := writer.GetText("t")
	writer.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 2, "cd", nil) })
	return first, crdt.EncodeStateAsUpdateV1(writer, before)
}

// relayHooks are the two ways a room applies a peer's update: without a
// hook, and through an OnPeerUpdate hook that applies it.
var relayHooks = map[string]func(*ygws.Server){
	"no hook": func(*ygws.Server) {},
	"OnPeerUpdate": func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error { return apply() }
	},
}

// Under RelayIntegratedOnly a frame that parks reaches no other peer, and the
// frame that fills its gap reaches them with what the room's drain landed, so
// they never hold, or park, what the room has not landed.
func TestInteg_RelayIntegratedOnly_AParkedFrameIsRelayedWithTheFrameThatLandsIt(t *testing.T) {
	for name, hook := range relayHooks {
		t.Run(name, func(t *testing.T) {
			srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
				srv.RelayIntegratedOnly = true
				hook(srv)
			})
			editor := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))
			watcherDoc := crdt.New(crdt.WithClientID(9))
			watcher := hpDial(t, ts, "room", false, watcherDoc)
			first, second := gappedEdits()

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(second))
			require.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
			require.Equal(t, 1, srv.GetDoc("room").PendingStats().Items, "the room parks the frame behind its gap")
			assert.Empty(t, watcherSyncFrames(t, watcher, watcherDoc), "a frame that only parked reaches no other peer")
			assert.Zero(t, watcherDoc.PendingStats().Items, "the watcher parks nothing the room has not landed")

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(first))
			require.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
			frames := watcherSyncFrames(t, watcher, watcherDoc)
			assert.Len(t, frames, 1, "the fill reaches the watcher as one update")
			assert.Equal(t, "abcd", watcherDoc.GetText("t").ToString(), "the fill carries what its drain landed")
			assert.Zero(t, watcherDoc.PendingStats().Items)

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(second))
			require.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
			assert.Empty(t, watcherSyncFrames(t, watcher, watcherDoc), "a frame the room holds is relayed to nobody")
		})
	}
}

// Off, the room relays the frame the peer sent, parked content included, and
// the other peers park it as the room does.
func TestInteg_RelayIntegratedOnly_OffRelaysTheFrameAsSent(t *testing.T) {
	_, ts := hocuspocusServer(t, nil)
	editor := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))
	watcherDoc := crdt.New(crdt.WithClientID(9))
	watcher := hpDial(t, ts, "room", false, watcherDoc)
	_, second := gappedEdits()

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(second))
	require.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
	assert.Len(t, watcherSyncFrames(t, watcher, watcherDoc), 1)
	assert.Equal(t, 1, watcherDoc.PendingStats().Items, "the watcher parks the relayed frame")
}

// Under RelayIntegratedOnly an update that fails partway relays what it
// integrated and nothing it parked.
func TestInteg_RelayIntegratedOnly_AFailedApplyRelaysWhatIntegrated(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) { srv.RelayIntegratedOnly = true })
	author := crdt.New(crdt.WithClientID(1))
	root := author.GetMap("m")
	author.Transact(func(txn *crdt.Transaction) { root.Set(txn, "nested", crdt.NewMapPrelim()) })
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	watcherDoc := crdt.New(crdt.WithClientID(9))
	watcher := hpDial(t, ts, "room", false, watcherDoc)

	// The text integrates and the map entries park, under a parent the room
	// lacks, before the delete set fails to decode.
	require.NoError(t, crdt.ApplyUpdateV1(editorDoc, crdt.EncodeStateAsUpdateV1(author, nil), nil))
	v, _ := editorDoc.GetMap("m").Get("nested")
	nested := v.(*crdt.YMap)
	txt := editorDoc.GetText("t")
	editorDoc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "abc", nil)
		nested.Set(txn, "k", 1)
	})
	cut := crdt.EncodeStateAsUpdateV1(editorDoc, author.StateVector())
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(cut[:len(cut)-1]))
	require.Equal(t, []uint64{0}, syncStatuses(t, editor, "room"))
	require.Equal(t, 1, srv.GetDoc("room").PendingStats().Items, "the room parks the map entry")

	watcherSyncFrames(t, watcher, watcherDoc)
	assert.Equal(t, "abc", watcherDoc.GetText("t").ToString(), "the watcher gets the text that integrated")
	assert.Zero(t, watcherDoc.PendingStats().Items, "the watcher is sent nothing that parked")
}

// Under AnswerParkedNotApplied a frame the room parks is answered 0, so the
// client keeps it to send again; the frame that fills its gap, and the parked
// frame sent again once the room holds it, are answered 1.
func TestInteg_AnswerParkedNotApplied_AParkedFrameIsAnsweredZeroUntilTheRoomHoldsIt(t *testing.T) {
	for name, hook := range relayHooks {
		t.Run(name, func(t *testing.T) {
			srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
				srv.AnswerParkedNotApplied = true
				hook(srv)
			})
			editor := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))
			first, second := gappedEdits()

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(second))
			assert.Equal(t, []uint64{0}, syncStatuses(t, editor, "room"), "a frame that parked is answered not applied")
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(second))
			assert.Equal(t, []uint64{0}, syncStatuses(t, editor, "room"), "sent again while it parks, it is answered not applied again")
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(first))
			assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"), "the fill lands both")
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(second))
			assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"), "the parked frame sent again once the room holds it")
			assert.Equal(t, "abcd", srv.GetDoc("room").GetText("t").ToString())
			assert.Zero(t, srv.GetDoc("room").PendingStats().Items)
		})
	}
}

// Under AnswerParkedNotApplied a frame part of which integrates and part of
// which parks is answered 0, and its part that integrated stands.
func TestInteg_AnswerParkedNotApplied_AFramePartlyParkedIsAnsweredZero(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) { srv.AnswerParkedNotApplied = true })
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)

	both := mergedUpdate(t, textUpdate(crdt.New(crdt.WithClientID(5)), "own"), parkingUpdate(t, 2))
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(both))
	assert.Equal(t, []uint64{0}, syncStatuses(t, editor, "room"))
	assert.Equal(t, "own", srv.GetDoc("room").GetText("t").ToString(), "the part that integrated stands")
	assert.Equal(t, 2, srv.GetDoc("room").PendingStats().Items)
}

// Off, a frame the room parks is answered 1, as @hocuspocus/server answers.
func TestInteg_AnswerParkedNotApplied_OffAnswersAParkedFrameApplied(t *testing.T) {
	_, ts := hocuspocusServer(t, nil)
	editor := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))
	_, second := gappedEdits()

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(second))
	assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
}

// mergedUpdate is one update carrying all of updates.
func mergedUpdate(t *testing.T, updates ...[]byte) []byte {
	t.Helper()
	u, err := crdt.MergeUpdatesV1(updates...)
	require.NoError(t, err)
	return u
}
