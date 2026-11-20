package websocket_test

import (
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// Tests for what AnswerOnceStored answers a frame that commits nothing: it
// takes the verdict of the room instance's newest store still running,
// whichever peer's commit that store holds, and once a commit of the room
// instance was not stored every answer the instance sends is 0.

// awaitRelay reads conn's frames into doc until doc holds everything from
// holds. A commit's store begins in the commit, and its relay goes out only
// once the apply returns (applySyncUpdate), so a store that has begun says
// nothing of whether the relay has reached another peer yet.
func awaitRelay(t *testing.T, conn *gws.Conn, doc, from *crdt.Doc) {
	t.Helper()
	update := crdt.EncodeStateAsUpdateV1(from, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		held, err := crdt.DocContainsUpdateV1(doc, update)
		require.NoError(t, err)
		if held {
			return
		}
		require.True(t, time.Now().Before(deadline), "the relay of the commit in flight never arrived")
		watcherSyncFrames(t, conn, doc)
	}
}

// A frame of another peer, or of a read-only peer, that carries only what a
// commit still being stored put in the room waits for that store, and is
// answered 1 only when it stored.
func TestInteg_AnswerOnceStored_AFrameThatCommitsNothingTakesTheVerdictOfTheStoreInFlight(t *testing.T) {
	for name, verdict := range map[string]storeVerdict{"stored": {stored: true}, "not stored": {stored: false}} {
		t.Run(name, func(t *testing.T) {
			var want uint64
			if verdict.stored {
				want = 1
			}
			gate := newGatedStore()
			_, ts := storeServer(t, storedGate{gate}, gate, func(srv *ygws.Server) {
				answerOnceStored(srv)
				srv.Authorize = readOnlyByHeader
			})
			editorDoc := crdt.New(crdt.WithClientID(7))
			editor := hpDial(t, ts, "room", false, editorDoc)
			otherDoc := crdt.New(crdt.WithClientID(9))
			other := hpDial(t, ts, "room", false, otherDoc)
			readerDoc := crdt.New(crdt.WithClientID(11))
			reader := hpDial(t, ts, "room", true, readerDoc)

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
			gate.waitStore(t)
			awaitRelay(t, other, otherDoc, editorDoc)
			awaitRelay(t, reader, readerDoc, editorDoc)
			require.Equal(t, "a", otherDoc.GetText("t").ToString(), "the relay does not wait for the store")
			hpSend(t, other, "room", tagSync, wholeStep2(t, otherDoc))
			hpSend(t, reader, "room", tagSync, wholeStep2(t, readerDoc))
			assert.Empty(t, syncStatuses(t, other, "room"), "a frame that commits nothing waits for the store in flight")
			assert.Empty(t, syncStatuses(t, reader, "room"), "a read-only peer's SyncStep2 the room holds waits too")

			gate.release <- verdict
			assert.Equal(t, []uint64{want}, statusesFor(t, editor, 1))
			assert.Equal(t, []uint64{want}, statusesFor(t, other, 1), "it takes that store's verdict")
			assert.Equal(t, []uint64{want}, statusesFor(t, reader, 1), "the read-only answer takes it too")
		})
	}
}

// The provider's reply to ResyncPeer that carries only what a commit still
// being stored put in the room waits for that store, as any frame that
// commits nothing does.
func TestInteg_AnswerOnceStored_TheReplyToResyncPeerTakesTheVerdictOfTheStoreInFlight(t *testing.T) {
	gate := newGatedStore()
	origins := make(chan any, 1)
	srv, ts := storeServer(t, storedGate{gate}, gate, func(srv *ygws.Server) {
		answerOnceStored(srv)
		originHook(origins)(srv)
	})
	otherDoc := crdt.New(crdt.WithClientID(9))
	other := hpDial(t, ts, "room", false, otherDoc)
	hpSend(t, other, "room", tagSync, ygsync.EncodeUpdate(textUpdate(otherDoc, "w")))
	gate.waitStore(t)
	gate.release <- storeVerdict{stored: true}
	require.Equal(t, []uint64{1}, statusesFor(t, other, 1))
	otherOrigin := <-origins

	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
	gate.waitStore(t)
	awaitRelay(t, other, otherDoc, editorDoc)
	require.True(t, srv.ResyncPeer(otherOrigin))
	_, step1 := syncEvents(t, other, 1)
	reply, err := ygsync.EncodeSyncStep2(otherDoc, step1)
	require.NoError(t, err)
	hpSend(t, other, "room", tagSync, reply)
	assert.Empty(t, syncStatuses(t, other, "room"), "the reply waits for the store in flight")

	gate.release <- storeVerdict{stored: false}
	assert.Equal(t, []uint64{0}, statusesFor(t, editor, 1))
	assert.Equal(t, []uint64{0}, statusesFor(t, other, 1), "the reply takes that store's verdict")
}

// Once a commit of a room instance was not stored, every answer the instance
// sends after it is 0: another peer's commit the adapter stores, and a frame
// that commits nothing with no store running. The room loaded again is a new
// instance, and answers as before.
func TestInteg_AnswerOnceStored_AfterACommitNotStoredEveryAnswerOfTheRoomIsZero(t *testing.T) {
	gate := newGatedStore()
	srv, ts := storeServer(t, storedGate{gate}, gate, answerOnceStored)
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	otherDoc := crdt.New(crdt.WithClientID(9))
	other := hpDial(t, ts, "room", false, otherDoc)

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
	gate.waitStore(t)
	gate.release <- storeVerdict{stored: false}
	require.Equal(t, []uint64{0}, statusesFor(t, editor, 1))

	hpSend(t, other, "room", tagSync, ygsync.EncodeUpdate(textUpdate(otherDoc, "b")))
	gate.waitStore(t)
	gate.release <- storeVerdict{stored: true}
	assert.Equal(t, []uint64{0}, statusesFor(t, other, 1), "another peer's commit, stored after the failure")
	hpSend(t, editor, "room", tagSync, wholeStep2(t, editorDoc))
	assert.Equal(t, []uint64{0}, statusesFor(t, editor, 1), "a frame that commits nothing, with no store running")

	require.NoError(t, srv.CloseRoom("room", true))
	againDoc := crdt.New(crdt.WithClientID(11))
	again := hpDial(t, ts, "room", false, againDoc)
	hpSend(t, again, "room", tagSync, ygsync.EncodeUpdate(textUpdate(againDoc, "c")))
	gate.waitStore(t)
	gate.release <- storeVerdict{stored: true}
	assert.Equal(t, []uint64{1}, statusesFor(t, again, 1), "the room loaded again starts clean")
}

// With ReadOnlySyncStep2MaxBytes set, a read-only peer's SyncStep2 whose
// update is longer than it is answered 0 without being decoded, under Sync
// and SyncReply alike, though the room holds all of it; one within it is
// still answered by whether the room holds it, and 0 sets no limit.
func TestInteg_ReadOnlySyncStep2MaxBytes_ALongerStep2IsAnsweredZeroUndecoded(t *testing.T) {
	readerDoc := crdt.New(crdt.WithClientID(2))
	hello := textUpdate(crdt.New(crdt.WithClientID(1)), "hello")
	require.NoError(t, crdt.ApplyUpdateV1(readerDoc, hello, nil))
	step2 := wholeStep2(t, readerDoc)
	_, update, err := ygsync.ReadSyncMessage(step2)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		limit int
		want  []uint64
	}{
		"within the limit": {limit: len(update), want: []uint64{1, 1}},
		"past the limit":   {limit: len(update) - 1, want: []uint64{0, 0}},
		"no limit":         {limit: 0, want: []uint64{1, 1}},
	} {
		t.Run(name, func(t *testing.T) {
			_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
				srv.Authorize = readOnlyByHeader
				srv.ReadOnlySyncStep2MaxBytes = tc.limit
			})
			editor := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(1)))
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(hello))
			require.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))

			reader := hpDial(t, ts, "room", true, crdt.New(crdt.WithClientID(2)))
			hpSend(t, reader, "room", tagSync, step2)
			hpSend(t, reader, "room", tagSyncReply, step2)
			assert.Equal(t, tc.want, syncStatuses(t, reader, "room"), "a SyncStep2 the room holds, under Sync and SyncReply")
		})
	}
}
