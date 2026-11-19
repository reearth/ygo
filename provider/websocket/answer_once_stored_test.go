package websocket_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
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

// Tests for Server.AnswerOnceStored, PersistenceAdapterStored and
// Server.ClosePeer: a peer is answered applied only once the room's
// persistence has stored what its update changed, in the order its frames
// arrived, while the update reaches the room's other peers at once.

// gatedStore is a persistence adapter whose every store waits for the test:
// each call sends its update on started, then returns what the test sends on
// release. Once open is closed every call returns at once, stored, so a room
// that closes at the end of a test is never left waiting.
type gatedStore struct {
	started chan []byte
	release chan storeVerdict
	open    chan struct{}
	once    sync.Once
}

// storeVerdict is what one store call returns: StoreUpdateStored's stored and
// err, and StoreUpdate's err.
type storeVerdict struct {
	stored bool
	err    error
}

func newGatedStore() *gatedStore {
	return &gatedStore{started: make(chan []byte, 16), release: make(chan storeVerdict), open: make(chan struct{})}
}

func (g *gatedStore) LoadDoc(string) ([]byte, error) { return nil, nil }

func (g *gatedStore) StoreUpdate(_ string, update []byte) error {
	_, err := g.store(update)
	return err
}

func (g *gatedStore) store(update []byte) (bool, error) {
	select {
	case g.started <- update:
	case <-g.open:
		return true, nil
	}
	select {
	case v := <-g.release:
		return v.stored, v.err
	case <-g.open:
		return true, nil
	}
}

// storedGate is gatedStore as a PersistenceAdapterStored: its verdict's stored
// says whether it stored each update.
type storedGate struct{ *gatedStore }

func (g storedGate) StoreUpdateStored(_ context.Context, _ string, update []byte) (bool, error) {
	return g.store(update)
}

// waitStore returns the update of the next store call to begin.
func (g *gatedStore) waitStore(t *testing.T) []byte {
	t.Helper()
	select {
	case u := <-g.started:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("no store call began")
		return nil
	}
}

// storeServer starts a Hocuspocus-framed server on adapter that stores each
// update in a call of its own, unless configure sets PersistCoalesceWindow.
// gate is opened before the server closes.
func storeServer(t *testing.T, adapter ygws.PersistenceAdapter, gate *gatedStore, configure func(*ygws.Server)) (*ygws.Server, *httptest.Server) {
	t.Helper()
	srv := ygws.NewServerWithPersistence(adapter)
	srv.HocuspocusFraming = true
	srv.PersistCoalesceWindow = -1
	if configure != nil {
		configure(srv)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { gate.once.Do(func() { close(gate.open) }) })
	return srv, ts
}

func answerOnceStored(srv *ygws.Server) { srv.AnswerOnceStored = true }

// statusesFor reads conn's frames until it has read n SyncStatus frames and
// returns their flags in order, skipping frames of other kinds.
func statusesFor(t *testing.T, conn *gws.Conn, n int) []uint64 {
	t.Helper()
	var flags []uint64
	for len(flags) < n {
		tag, body, err := hpRead(conn, "room", 5*time.Second)
		require.NoError(t, err)
		if tag == tagSyncStatus {
			flag, err := encoding.NewDecoder(body).ReadVarUint()
			require.NoError(t, err)
			flags = append(flags, flag)
		}
	}
	return flags
}

// closeAfter reads conn's frames until the connection closes and returns the
// flags of the SyncStatus frames read before the close, and the close's code
// and reason.
func closeAfter(t *testing.T, conn *gws.Conn) (flags []uint64, code int, reason string) {
	t.Helper()
	for {
		tag, body, err := hpRead(conn, "room", 5*time.Second)
		if err != nil {
			var closed *gws.CloseError
			require.ErrorAs(t, err, &closed, "the connection ends with a close frame")
			return flags, closed.Code, closed.Text
		}
		if tag == tagSyncStatus {
			flag, err := encoding.NewDecoder(body).ReadVarUint()
			require.NoError(t, err)
			flags = append(flags, flag)
		}
	}
}

// syncEvents reads conn's frames until it has read n SyncStatus frames and
// SyncStep1 messages together, and returns them in order, "status:<flag>" or
// "step1", with the last SyncStep1 message read.
func syncEvents(t *testing.T, conn *gws.Conn, n int) (events []string, step1 []byte) {
	t.Helper()
	for len(events) < n {
		tag, body, err := hpRead(conn, "room", 5*time.Second)
		require.NoError(t, err)
		switch tag {
		case tagSyncStatus:
			flag, err := encoding.NewDecoder(body).ReadVarUint()
			require.NoError(t, err)
			events = append(events, "status:"+strconv.FormatUint(flag, 10))
		case tagSync:
			if sub, _, err := ygsync.ReadSyncMessage(body); err == nil && sub == ygsync.MsgSyncStep1 {
				events, step1 = append(events, "step1"), body
			}
		}
	}
	return events, step1
}

// storeHooks are the two ways a room applies a peer's update: without a
// hook, and through an OnPeerUpdate hook that applies it.
var storeHooks = map[string]func(*ygws.Server){
	"no hook": func(*ygws.Server) {},
	"OnPeerUpdate": func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error { return apply() }
	},
}

// The answer to a frame waits until the adapter has stored what the frame
// changed, and the answers go out in the order the frames arrived: a frame
// whose apply changed nothing is answered at once only when no answer before
// it is still waiting.
func TestInteg_AnswerOnceStored_TheAnswerWaitsForTheStoreInFrameOrder(t *testing.T) {
	for name, hook := range storeHooks {
		t.Run(name, func(t *testing.T) {
			gate := newGatedStore()
			srv, ts := storeServer(t, storedGate{gate}, gate, func(srv *ygws.Server) {
				answerOnceStored(srv)
				hook(srv)
			})
			editorDoc := crdt.New(crdt.WithClientID(7))
			editor := hpDial(t, ts, "room", false, editorDoc)
			first := textUpdate(editorDoc, "a")
			second := textUpdate(editorDoc, "b")

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(first))
			gate.waitStore(t)
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(first))
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(second))
			assert.Empty(t, syncStatuses(t, editor, "room"),
				"no answer while the first frame's store runs, the held resend's included")
			assert.Equal(t, "ba", srv.GetDoc("room").GetText("t").ToString(), "the room applied all three")

			gate.release <- storeVerdict{stored: true}
			assert.Equal(t, []uint64{1, 1}, statusesFor(t, editor, 2), "the first frame, then the resend behind it")
			gate.waitStore(t)
			assert.Empty(t, syncStatuses(t, editor, "room"), "the third frame's answer waits for its own store")

			gate.release <- storeVerdict{stored: true}
			assert.Equal(t, []uint64{1}, statusesFor(t, editor, 1))
		})
	}
}

// An update the adapter did not store - its call failed, or a
// PersistenceAdapterStored adapter says it dropped it - is answered 0, and
// what it changed stands in the room. Every later answer of the room
// instance is 0 too, a later update the adapter stores included: the room
// holds content no store holds.
func TestInteg_AnswerOnceStored_AnUpdateNotStoredIsAnsweredZero(t *testing.T) {
	for name, tc := range map[string]struct {
		adapter func(*gatedStore) ygws.PersistenceAdapter
		verdict storeVerdict
	}{
		"the store fails": {
			adapter: func(g *gatedStore) ygws.PersistenceAdapter { return g },
			verdict: storeVerdict{err: errors.New("disk full")},
		},
		"the adapter says it did not store it": {
			adapter: func(g *gatedStore) ygws.PersistenceAdapter { return storedGate{g} },
			verdict: storeVerdict{stored: false},
		},
	} {
		t.Run(name, func(t *testing.T) {
			gate := newGatedStore()
			srv, ts := storeServer(t, tc.adapter(gate), gate, answerOnceStored)
			editorDoc := crdt.New(crdt.WithClientID(7))
			editor := hpDial(t, ts, "room", false, editorDoc)

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
			gate.waitStore(t)
			gate.release <- tc.verdict
			assert.Equal(t, []uint64{0}, statusesFor(t, editor, 1))
			assert.Equal(t, "a", srv.GetDoc("room").GetText("t").ToString(), "the update stands in the room")

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "b")))
			gate.waitStore(t)
			gate.release <- storeVerdict{stored: true}
			assert.Equal(t, []uint64{0}, statusesFor(t, editor, 1), "a later update stored is answered 0 too")
		})
	}
}

// Only the sender's answer waits: the update reaches the room's other peers
// as soon as the room applied it, before the adapter has stored it.
func TestInteg_AnswerOnceStored_TheRelayGoesOutBeforeTheStore(t *testing.T) {
	gate := newGatedStore()
	_, ts := storeServer(t, gate, gate, answerOnceStored)
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	watcherDoc := crdt.New(crdt.WithClientID(9))
	watcher := hpDial(t, ts, "room", false, watcherDoc)

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
	gate.waitStore(t)
	assert.Len(t, watcherSyncFrames(t, watcher, watcherDoc), 1, "the relay does not wait for the store")
	assert.Equal(t, "a", watcherDoc.GetText("t").ToString())
	assert.Empty(t, syncStatuses(t, editor, "room"), "the sender's answer does")

	gate.release <- storeVerdict{stored: true}
	assert.Equal(t, []uint64{1}, statusesFor(t, editor, 1))
}

// With persistence coalescing, the answers to the frames one merged write
// stores wait for that write.
func TestInteg_AnswerOnceStored_ACoalescedWriteAnswersEveryFrameItStores(t *testing.T) {
	gate := newGatedStore()
	srv, ts := storeServer(t, storedGate{gate}, gate, func(srv *ygws.Server) {
		answerOnceStored(srv)
		srv.PersistCoalesceWindow = time.Second
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "b")))
	merged := gate.waitStore(t)
	assert.Empty(t, syncStatuses(t, editor, "room"))
	stored := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(stored, merged, nil))
	assert.Equal(t, srv.GetDoc("room").GetText("t").ToString(), stored.GetText("t").ToString(),
		"one write stores both frames")

	gate.release <- storeVerdict{stored: true}
	assert.Equal(t, []uint64{1, 1}, statusesFor(t, editor, 2))
}

// Off, a peer is answered as soon as the room applied its update, while the
// adapter is still storing it.
func TestInteg_AnswerOnceStored_OffAnswersOnceTheRoomApplied(t *testing.T) {
	gate := newGatedStore()
	_, ts := storeServer(t, storedGate{gate}, gate, nil)
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
	gate.waitStore(t)
	assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
	gate.release <- storeVerdict{stored: false}
}

// Under AnswerOnceStored, ClosePeer's close goes out after the answers the
// peer is owed for the frames it sent before the call.
func TestInteg_AnswerOnceStored_ClosePeerGoesOutAfterTheAnswersOwed(t *testing.T) {
	gate := newGatedStore()
	origins := make(chan any, 1)
	srv, ts := storeServer(t, gate, gate, func(srv *ygws.Server) {
		answerOnceStored(srv)
		srv.OnPeerUpdate = func(info ygws.PeerUpdateInfo, apply func() error) error {
			origins <- info.Origin
			return apply()
		}
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
	gate.waitStore(t)
	assert.Empty(t, syncStatuses(t, editor, "room"))
	require.True(t, srv.ClosePeer(<-origins, 4503, "busy"))

	gate.release <- storeVerdict{stored: true}
	flags, code, reason := closeAfter(t, editor)
	assert.Equal(t, []uint64{1}, flags, "the answer owed goes out before the close")
	assert.Equal(t, 4503, code)
	assert.Equal(t, "busy", reason)
}

// ClosePeer closes the peer an update's origin names, with the code and
// reason given, and reports whether that origin was an open peer of the
// server; a code ServeHTTP would not send closes with 4205.
func TestUnit_Server_ClosePeer_ClosesThePeerAnUpdateCameFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		code       int
		reason     string
		wantCode   int
		wantReason string
	}{
		"a sendable code":   {code: 4503, reason: "busy", wantCode: 4503, wantReason: "busy"},
		"an unsendable one": {code: 1011, reason: "busy", wantCode: 4205, wantReason: "Reset Connection"},
	} {
		t.Run(name, func(t *testing.T) {
			origins := make(chan any, 1)
			srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
				srv.OnPeerUpdate = func(info ygws.PeerUpdateInfo, apply func() error) error {
					origins <- info.Origin
					return apply()
				}
			})
			editorDoc := crdt.New(crdt.WithClientID(7))
			editor := hpDial(t, ts, "room", false, editorDoc)
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
			require.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
			origin := <-origins

			assert.False(t, srv.ClosePeer("not a peer", tc.code, tc.reason))
			assert.False(t, ygws.NewServer().ClosePeer(origin, tc.code, tc.reason), "a peer of another server")
			require.True(t, srv.ClosePeer(origin, tc.code, tc.reason))
			assert.False(t, srv.ClosePeer(origin, tc.code, tc.reason), "a peer already closing")
			_, code, reason := closeAfter(t, editor)
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}

// originHook configures a server whose OnPeerUpdate hook hands the test the
// origin of an update, on origins when it has room, and applies it.
func originHook(origins chan any) func(*ygws.Server) {
	return func(srv *ygws.Server) {
		srv.OnPeerUpdate = func(info ygws.PeerUpdateInfo, apply func() error) error {
			select {
			case origins <- info.Origin:
			default:
			}
			return apply()
		}
	}
}

// ResyncPeer sends the peer an update's origin names the room's SyncStep1,
// which the peer's provider answers with a SyncStep2 that the room answers
// applied. It reports false for an origin that is no open peer of the
// server: not a peer, another server's, or one gone.
func TestUnit_Server_ResyncPeer_SendsThePeerTheRoomsSyncStep1(t *testing.T) {
	origins := make(chan any, 1)
	srv, ts := hocuspocusServer(t, originHook(origins))
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
	require.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))
	origin := <-origins

	assert.False(t, srv.ResyncPeer("not a peer"))
	assert.False(t, ygws.NewServer().ResyncPeer(origin), "a peer of another server")
	require.True(t, srv.ResyncPeer(origin))
	events, step1 := syncEvents(t, editor, 1)
	assert.Equal(t, []string{"step1"}, events)
	assert.Equal(t, ygsync.EncodeSyncStep1(srv.GetDoc("room")), step1, "the room's state vector")

	reply, err := ygsync.EncodeSyncStep2(editorDoc, step1)
	require.NoError(t, err)
	hpSend(t, editor, "room", tagSync, reply)
	assert.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"), "the provider's SyncStep2 is answered applied")

	require.NoError(t, editor.Close())
	assert.Eventually(t, func() bool { return !srv.ResyncPeer(origin) }, 5*time.Second, 10*time.Millisecond,
		"a peer gone answers false")
}

// Under AnswerOnceStored, ResyncPeer's SyncStep1 goes out after the answers
// the peer is owed for the frames it sent before the call.
func TestInteg_AnswerOnceStored_ResyncPeerGoesOutAfterTheAnswersOwed(t *testing.T) {
	gate := newGatedStore()
	origins := make(chan any, 1)
	srv, ts := storeServer(t, gate, gate, func(srv *ygws.Server) {
		answerOnceStored(srv)
		originHook(origins)(srv)
	})
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)

	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
	gate.waitStore(t)
	assert.Empty(t, syncStatuses(t, editor, "room"))
	require.True(t, srv.ResyncPeer(<-origins))

	gate.release <- storeVerdict{stored: true}
	events, step1 := syncEvents(t, editor, 2)
	assert.Equal(t, []string{"status:1", "step1"}, events, "the answer owed, then the SyncStep1")
	assert.Equal(t, ygsync.EncodeSyncStep1(srv.GetDoc("room")), step1)
}

// ResyncPeer may be called from any goroutine, from inside a transaction on
// the room's document included, and each call's SyncStep1 reaches the peer.
func TestUnit_Server_ResyncPeer_IsSafeFromAnyGoroutine(t *testing.T) {
	for name, configure := range map[string]func(*ygws.Server){
		"off":              func(*ygws.Server) {},
		"AnswerOnceStored": answerOnceStored,
	} {
		t.Run(name, func(t *testing.T) {
			origins := make(chan any, 1)
			srv, ts := storeServer(t, ygws.NewMemoryPersistence(), newGatedStore(), func(srv *ygws.Server) {
				configure(srv)
				originHook(origins)(srv)
			})
			editorDoc := crdt.New(crdt.WithClientID(7))
			editor := hpDial(t, ts, "room", false, editorDoc)
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "a")))
			require.Equal(t, []uint64{1}, statusesFor(t, editor, 1))
			origin := <-origins

			var inside bool
			srv.GetDoc("room").Transact(func(*crdt.Transaction) { inside = srv.ResyncPeer(origin) })
			require.True(t, inside, "from inside a transaction on the room's document")
			const n = 8
			var wg sync.WaitGroup
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					assert.True(t, srv.ResyncPeer(origin))
				}()
			}
			wg.Wait()
			events, _ := syncEvents(t, editor, n+1)
			assert.Equal(t, slices.Repeat([]string{"step1"}, n+1), events)
		})
	}
}
