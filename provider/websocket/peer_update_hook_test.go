package websocket_test

import (
	"errors"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
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

// closeAfterStatuses reads frames until the connection closes, or until a
// Pong when it stays open, and returns the flag of every SyncStatus on the way
// and the close error (nil when a Pong came instead). Other frames are
// skipped.
func closeAfterStatuses(t *testing.T, conn *gws.Conn, room string) ([]uint64, *gws.CloseError) {
	t.Helper()
	var flags []uint64
	for {
		tag, body, err := hpRead(conn, room, 2*time.Second)
		if err != nil {
			var closed *gws.CloseError
			require.ErrorAs(t, err, &closed, "the connection must end with a close frame")
			return flags, closed
		}
		switch tag {
		case tagPong:
			return flags, nil
		case tagSyncStatus:
			flag, err := encoding.NewDecoder(body).ReadVarUint()
			require.NoError(t, err)
			flags = append(flags, flag)
		}
	}
}

// An update the hook refuses with a plain error is not in the room, reaches
// no other peer, and is answered SyncStatus(0); the peer is then closed with
// 4205 (Reset Connection), as @hocuspocus/server closes on any
// beforeHandleMessage rejection. The same client's next edit, which builds on
// the refused one, never reaches the room: without the close it would park
// there and be answered SyncStatus(1).
func TestInteg_OnPeerUpdate_RefusalClosesWithResetConnection(t *testing.T) {
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
	// The editor's close leaves the watcher in the room, so it stays loaded.
	room := srv.GetDoc("room")

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "refused")))
	tag, body, err := hpRead(editor, "room", 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, tagSyncStatus, tag)
	require.Equal(t, []byte{0}, body)
	// The same client's next edit. A write can fail once the close is under
	// way; the close below is what matters.
	_ = editor.WriteMessage(gws.BinaryMessage, hpFrame("room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "next"))))
	_ = editor.WriteMessage(gws.BinaryMessage, hpFrame("room", tagPing, nil))
	statuses, closed := closeAfterStatuses(t, editor, "room")
	assert.Empty(t, statuses, "the next edit must not be answered")
	require.NotNil(t, closed, "the refusal must close the connection")
	assert.Equal(t, 4205, closed.Code)
	assert.Equal(t, "Reset Connection", closed.Text)
	assert.Equal(t, int32(1), calls.Load(), "the next edit must not reach the hook")

	assert.Empty(t, room.GetText("t").ToString(), "a refused update must not reach the room")
	assert.Zero(t, room.PendingStats().Items, "the client's next edit must not park in the room")
	assert.NotContains(t, tagsUntilPong(t, watcher, "room"), tagSync, "a refused update must reach no other peer")
}

// A plain y-websocket connection, which has no SyncStatus, is closed the same
// way on a refusal.
func TestInteg_OnPeerUpdate_RefusalClosesAYWebsocketPeer(t *testing.T) {
	srv := ygws.NewServer()
	srv.OnPeerUpdate = func(ygws.PeerUpdateInfo, func() error) error { return errors.New("refused") }
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	doc := crdt.New(crdt.WithClientID(7))
	conn := dial(t, ts, "room")
	drainHandshake(t, conn, doc)

	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(tagSync)
		enc.WriteRaw(ygsync.EncodeUpdate(textUpdate(doc, "x")))
	})))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	var closed *gws.CloseError
	require.ErrorAs(t, err, &closed)
	assert.Equal(t, 4205, closed.Code)
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
	require.ErrorIs(t, second, ygws.ErrPeerUpdateApplied)
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

// Frames that arrived behind a refused update are not handled once the close
// is queued: they reach neither the hook nor the room, however long the hook
// took to refuse.
func TestInteg_OnPeerUpdate_FramesBehindARefusalAreDropped(t *testing.T) {
	tests := []struct {
		name     string
		refusal  error
		wantCode int
	}{
		{name: "PeerClose", refusal: &ygws.PeerClose{Code: 4413, Reason: "too large"}, wantCode: 4413},
		{name: "plain error", refusal: errors.New("refused"), wantCode: 4205},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
				srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error {
					if calls.Add(1) == 1 {
						time.Sleep(50 * time.Millisecond)
						return tt.refusal
					}
					return apply()
				}
			})
			doc := crdt.New(crdt.WithClientID(7))
			conn := hpDial(t, ts, "room", false, doc)
			// The close leaves the room with no peer, which evicts it, so its
			// document is taken first.
			room := srv.GetDoc("room")

			hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "refused")))
			// Five updates the room could take on their own, each from a
			// client of its own, sent while the hook is still deciding.
			for i := range 5 {
				other := crdt.New(crdt.WithClientID(crdt.ClientID(100 + i)))
				hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(other, "z")))
			}
			statuses, closed := closeAfterStatuses(t, conn, "room")
			assert.Equal(t, []uint64{0}, statuses, "only the refused update is answered")
			require.NotNil(t, closed, "the refusal must close the connection")
			assert.Equal(t, tt.wantCode, closed.Code)
			assert.Equal(t, int32(1), calls.Load(), "the hook runs once")
			assert.Empty(t, room.GetText("t").ToString(), "nothing behind the refusal lands")
		})
	}
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

// A *PeerClose code outside 1000 and 3000-4999 closes with 4205 instead; those
// are sent as given, the rule ServeHTTP applies to a context cause's code.
func TestInteg_OnPeerUpdate_PeerCloseCodeOutsideTheSendableRangeIsReset(t *testing.T) {
	tests := []struct {
		code     int
		wantCode int
		wantText string
	}{
		{code: 4413, wantCode: 4413, wantText: "custom"},
		{code: 3000, wantCode: 3000, wantText: "custom"},
		{code: 1000, wantCode: 1000, wantText: "custom"},
		{code: 1001, wantCode: 4205, wantText: "Reset Connection"},
		{code: 2999, wantCode: 4205, wantText: "Reset Connection"},
		{code: 5000, wantCode: 4205, wantText: "Reset Connection"},
		{code: 0, wantCode: 4205, wantText: "Reset Connection"},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.code), func(t *testing.T) {
			_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
				srv.OnPeerUpdate = func(ygws.PeerUpdateInfo, func() error) error {
					return &ygws.PeerClose{Code: tt.code, Reason: "custom"}
				}
			})
			doc := crdt.New(crdt.WithClientID(7))
			conn := hpDial(t, ts, "room", false, doc)

			hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "x")))
			_ = conn.WriteMessage(gws.BinaryMessage, hpFrame("room", tagPing, nil))
			statuses, closed := closeAfterStatuses(t, conn, "room")
			assert.Equal(t, []uint64{0}, statuses)
			require.NotNil(t, closed, "the refusal must close the connection")
			assert.Equal(t, tt.wantCode, closed.Code)
			assert.Equal(t, tt.wantText, closed.Text)
		})
	}
}

// When the hook calls apply and the room itself does not take the update (it
// overflows MaxPendingItems), the peer is answered as without a hook:
// SyncStatus(0), and the connection stays, whether the hook returns apply's
// error or not.
func TestInteg_OnPeerUpdate_RoomRefusalThroughApplyKeepsTheConnection(t *testing.T) {
	var applyErr error
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.MaxPendingItems = 2
		srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error {
			applyErr = apply()
			return applyErr
		}
	})
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(parkingUpdate(t, 5)))
	assert.Equal(t, []uint64{0}, syncStatuses(t, conn, "room"), "the connection stays")
	assert.Error(t, applyErr)
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

// A hook that panics before applying refuses the update and, like a plain
// error, closes the peer with 4205.
func TestInteg_OnPeerUpdate_PanicBeforeApplyRefusesAndCloses(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(ygws.PeerUpdateInfo, func() error) error { panic("boom") }
	})
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)
	room := srv.GetDoc("room")

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "x")))
	_ = conn.WriteMessage(gws.BinaryMessage, hpFrame("room", tagPing, nil))
	statuses, closed := closeAfterStatuses(t, conn, "room")
	assert.Equal(t, []uint64{0}, statuses)
	require.NotNil(t, closed, "the refusal must close the connection")
	assert.Equal(t, 4205, closed.Code)
	assert.Empty(t, room.GetText("t").ToString())
}

// An apply called after the hook returned applies nothing and returns
// ErrPeerUpdateHookReturned: by then the peer has been answered and the update
// was not broadcast, so applying it would leave the room holding an edit no
// other peer has.
func TestInteg_OnPeerUpdate_ApplyAfterReturnAppliesNothing(t *testing.T) {
	late := make(chan error, 1)
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error {
			go func() {
				time.Sleep(20 * time.Millisecond)
				late <- apply()
			}()
			return nil
		}
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	watcher := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))
	room := srv.GetDoc("room")

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "late")))
	_ = editor.WriteMessage(gws.BinaryMessage, hpFrame("room", tagPing, nil))
	statuses, _ := closeAfterStatuses(t, editor, "room")
	assert.Equal(t, []uint64{0}, statuses)
	select {
	case err := <-late:
		require.ErrorIs(t, err, ygws.ErrPeerUpdateHookReturned)
	case <-time.After(2 * time.Second):
		t.Fatal("the late apply never returned")
	}
	assert.Empty(t, room.GetText("t").ToString(), "a late apply must not reach the room")
	assert.NotContains(t, tagsUntilPong(t, watcher, "room"), tagSync)
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

// An apply called while the first one is still running, from a document
// observer that apply fires, returns ErrPeerUpdateApplied instead of waiting
// on the first: the update is applied once and the room's later writes go
// through.
func TestInteg_OnPeerUpdate_ApplyFromAnObserverOfTheApplyReturnsApplied(t *testing.T) {
	nested := make(chan error, 1)
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(info ygws.PeerUpdateInfo, apply func() error) error {
			unsubscribe := info.Doc.OnUpdate(func([]byte, any) {
				select {
				case nested <- apply():
				default:
				}
			})
			defer unsubscribe()
			return apply()
		}
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "once")))
	select {
	case err := <-nested:
		require.ErrorIs(t, err, ygws.ErrPeerUpdateApplied)
	case <-time.After(2 * time.Second):
		t.Fatal("the nested apply never returned")
	}
	assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
	assert.Equal(t, "once", srv.GetDoc("room").GetText("t").ToString())
}

// A nil *PeerClose the hook returns as its error refuses like a plain error:
// the peer is closed with 4205.
func TestInteg_OnPeerUpdate_NilPeerCloseClosesWithResetConnection(t *testing.T) {
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(ygws.PeerUpdateInfo, func() error) error {
			var refusal *ygws.PeerClose
			return refusal
		}
	})
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "x")))
	_ = conn.WriteMessage(gws.BinaryMessage, hpFrame("room", tagPing, nil))
	statuses, closed := closeAfterStatuses(t, conn, "room")
	assert.Equal(t, []uint64{0}, statuses)
	require.NotNil(t, closed, "the refusal must close the connection")
	assert.Equal(t, 4205, closed.Code)
	assert.NotPanics(t, func() { _ = (*ygws.PeerClose)(nil).Error() })
}

// An apply the hook started but that is still running when the hook returns
// finishes before the peer is answered. The observer holds the apply open until
// the test has seen no answer arrive, so without the wait the test fails.
func TestInteg_OnPeerUpdate_AnswerWaitsForAnApplyStillRunning(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseApply := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseApply)
	hookReturned := make(chan struct{})
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(info ygws.PeerUpdateInfo, apply func() error) error {
			started := make(chan struct{})
			var once sync.Once
			info.Doc.OnUpdate(func([]byte, any) {
				once.Do(func() {
					close(started)
					<-release
				})
			})
			go func() { _ = apply() }()
			<-started
			close(hookReturned)
			return nil
		}
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "slow")))
	select {
	case <-hookReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("the hook never returned")
	}
	answered := make(chan uint64, 1)
	go func() {
		defer close(answered)
		for {
			tag, body, err := hpRead(editor, "room", 5*time.Second)
			if err != nil {
				return
			}
			if tag == tagSyncStatus {
				if flag, err := encoding.NewDecoder(body).ReadVarUint(); err == nil {
					answered <- flag
				}
				return
			}
		}
	}()
	// The window only bounds the search for an early answer: with the wait, no
	// answer can arrive before the release, so a slow machine cannot fail it.
	select {
	case flag := <-answered:
		t.Fatalf("the peer was answered %d before its apply finished", flag)
	case <-time.After(100 * time.Millisecond):
	}
	releaseApply()
	flag, ok := <-answered
	require.True(t, ok, "no SyncStatus after the apply finished")
	assert.Equal(t, uint64(1), flag)
	assert.Equal(t, "slow", srv.GetDoc("room").GetText("t").ToString())
}
