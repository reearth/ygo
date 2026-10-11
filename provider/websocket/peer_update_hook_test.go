package websocket_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// Tests for Server.OnPeerUpdate, which runs in place of the room's own apply
// of a read-write peer's SyncStep2 or Update.

// tagsUntilPong sends a Ping and returns the outer tag of every frame read
// before its Pong.
func tagsUntilPong(t *testing.T, conn *gws.Conn, room string) []uint64 {
	t.Helper()
	hpSend(t, conn, room, tagPing, nil)
	var tags []uint64
	for {
		tag, _, err := hpRead(conn, room, 2*time.Second)
		require.NoError(t, err)
		if tag == tagPong {
			return tags
		}
		tags = append(tags, tag)
	}
}

// An update the hook does not apply is not in the room, reaches no other
// peer, and is answered SyncStatus(0); the socket stays open, and the next
// update the hook applies is answered SyncStatus(1).
func TestInteg_OnPeerUpdate_RefusedUpdateIsNotAppliedNorBroadcast(t *testing.T) {
	var calls atomic.Int32
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error {
			if calls.Add(1) == 1 {
				return errors.New("refused")
			}
			return apply()
		}
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	watcher := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "refused")))
	assert.Equal(t, []uint64{0}, syncStatuses(t, editor, "room"))
	assert.Empty(t, srv.GetDoc("room").GetText("t").ToString(), "a refused update must not reach the room")
	assert.NotContains(t, tagsUntilPong(t, watcher, "room"), tagSync, "a refused update must reach no other peer")

	fresh := crdt.New(crdt.WithClientID(9))
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(fresh, "taken")))
	assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
	assert.Equal(t, "taken", srv.GetDoc("room").GetText("t").ToString())
	assert.Contains(t, tagsUntilPong(t, watcher, "room"), tagSync, "an applied update reaches the other peers")
}

// The hook is handed the room, its document and the update's V1 bytes; apply
// applies the update exactly once, however often the hook calls it, and an
// applied update is broadcast and answered SyncStatus(1).
func TestInteg_OnPeerUpdate_ApplyRunsOnce(t *testing.T) {
	var (
		seen   ygws.PeerUpdateInfo
		second error
	)
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(info ygws.PeerUpdateInfo, apply func() error) error {
			seen = info
			if err := apply(); err != nil {
				return err
			}
			second = apply()
			return nil
		}
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	watcher := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))
	var applied atomic.Int32
	unsubscribe := srv.GetDoc("room").OnUpdate(func([]byte, any) { applied.Add(1) })
	defer unsubscribe()

	update := textUpdate(editorDoc, "hello")
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(update))
	assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
	assert.Equal(t, "hello", srv.GetDoc("room").GetText("t").ToString())
	assert.Equal(t, int32(1), applied.Load(), "the room applies the update once")
	assert.ErrorIs(t, second, ygws.ErrPeerUpdateApplied)
	assert.Equal(t, "room", seen.Room)
	assert.Same(t, srv.GetDoc("room"), seen.Doc)
	assert.Equal(t, update, seen.Update)
	assert.Contains(t, tagsUntilPong(t, watcher, "room"), tagSync)
}

// A hook that refuses with a *PeerClose closes the peer with its code and
// reason after the SyncStatus(0).
func TestInteg_OnPeerUpdate_PeerCloseClosesWithItsCode(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(ygws.PeerUpdateInfo, func() error) error {
			return &ygws.PeerClose{Code: 4413, Reason: "document too large"}
		}
	})
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)
	// The close leaves the room with no peer, which evicts it, so its document
	// is taken first.
	room := srv.GetDoc("room")

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "big")))
	tag, body, err := hpRead(conn, "room", 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, tagSyncStatus, tag, "the SyncStatus comes before the close")
	assert.Equal(t, []byte{0}, body)
	_, _, err = hpRead(conn, "room", 2*time.Second)
	var closed *gws.CloseError
	require.ErrorAs(t, err, &closed)
	assert.Equal(t, 4413, closed.Code)
	assert.Equal(t, "document too large", closed.Text)
	assert.Empty(t, room.GetText("t").ToString(), "a refused update must not reach the room")
}

// A close reason longer than a close frame carries is cut at a rune boundary,
// so the close still reaches the peer.
func TestInteg_OnPeerUpdate_LongCloseReasonIsCut(t *testing.T) {
	long := ""
	for range 200 {
		long += "é"
	}
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(ygws.PeerUpdateInfo, func() error) error {
			return &ygws.PeerClose{Code: 4413, Reason: long}
		}
	})
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "big")))
	var closed *gws.CloseError
	for {
		_, _, err := hpRead(conn, "room", 2*time.Second)
		if err != nil {
			require.ErrorAs(t, err, &closed)
			break
		}
	}
	assert.Equal(t, 4413, closed.Code)
	assert.LessOrEqual(t, len(closed.Text), 123)
	assert.Equal(t, long[:122], closed.Text, "cut on the last whole rune within the frame")
}

// Once apply has succeeded the update is in the room, so it is broadcast and
// answered SyncStatus(1) whatever the hook returns after it.
func TestInteg_OnPeerUpdate_AnAppliedUpdateIsBroadcastWhateverTheHookReturns(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error {
			require.NoError(t, apply())
			return &ygws.PeerClose{Code: 4413, Reason: "late"}
		}
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	watcher := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "kept")))
	assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"), "the socket stays open")
	assert.Equal(t, "kept", srv.GetDoc("room").GetText("t").ToString())
	assert.Contains(t, tagsUntilPong(t, watcher, "room"), tagSync)
}

// A hook that panics before applying refuses the update; the socket stays.
func TestInteg_OnPeerUpdate_PanicBeforeApplyRefuses(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(ygws.PeerUpdateInfo, func() error) error { panic("boom") }
	})
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "x")))
	assert.Equal(t, []uint64{0}, syncStatuses(t, conn, "room"))
	assert.Empty(t, srv.GetDoc("room").GetText("t").ToString())
}

// A read-only peer's update never reaches the hook: it is dropped as before.
func TestInteg_OnPeerUpdate_NotCalledForReadOnlyPeers(t *testing.T) {
	var calls atomic.Int32
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.Authorize = readOnlyByHeader
		srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error {
			calls.Add(1)
			return apply()
		}
	})
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", true, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "x")))
	assert.Equal(t, []uint64{0}, syncStatuses(t, conn, "room"))
	assert.Zero(t, calls.Load())
	assert.Empty(t, srv.GetDoc("room").GetText("t").ToString())
}

// With no hook a peer's update is applied, broadcast and answered as before.
func TestInteg_OnPeerUpdate_NilHookAppliesAsBefore(t *testing.T) {
	srv, ts := hocuspocusServer(t, nil)
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	watcher := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))

	hpSend(t, editor, "room", tagSyncReply, ygsync.EncodeUpdate(textUpdate(editorDoc, "plain")))
	assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
	assert.Equal(t, "plain", srv.GetDoc("room").GetText("t").ToString())
	assert.Contains(t, tagsUntilPong(t, watcher, "room"), tagSync)
}
