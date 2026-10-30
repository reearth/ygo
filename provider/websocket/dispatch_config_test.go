package websocket_test

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

// storeLog is a PersistenceAdapter that loads no stored state and keeps every
// update the room stores, in order.
type storeLog struct {
	mu      sync.Mutex
	updates [][]byte
}

func (a *storeLog) LoadDoc(string) ([]byte, error) { return nil, nil }

func (a *storeLog) StoreUpdate(_ string, update []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, append([]byte(nil), update...))
	return nil
}

func (a *storeLog) stored() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]byte(nil), a.updates...)
}

// dispatchServer starts a Hocuspocus-framed server that stores every update
// the room commits with one StoreUpdate each (PersistCoalesceWindow < 0), with
// a pending cap of limit, as a production deployment that persists through
// NewServerWithPersistence configures it, and returns its store.
func dispatchServer(t *testing.T, limit int) (*ygws.Server, *httptest.Server, *storeLog) {
	t.Helper()
	adapter := &storeLog{}
	srv := ygws.NewServerWithPersistence(adapter)
	srv.HocuspocusFraming = true
	srv.PersistCoalesceWindow = -1
	srv.MaxPendingItems = limit
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts, adapter
}

// relayedUpdates sends a Ping on conn and reads frames until its Pong,
// returning the update each document-update (Sync) frame on the way carries.
func relayedUpdates(t *testing.T, conn *gws.Conn, room string) [][]byte {
	t.Helper()
	hpSend(t, conn, room, tagPing, nil)
	var updates [][]byte
	for {
		tag, body, err := hpRead(conn, room, 2*time.Second)
		require.NoError(t, err)
		switch tag {
		case tagPong:
			return updates
		case tagSync:
			_, update, err := ygsync.ReadSyncMessage(body)
			require.NoError(t, err)
			updates = append(updates, update)
		}
	}
}

// A room under that configuration (dispatchServer) refuses an update that
// would park more items than the cap without changing anything: the update
// inserts "hello" and sets five keys on a map whose creator the room never
// heard from, with a cap of two. The room's state and text are as they were,
// its OnUpdate observers are handed nothing, nothing is stored, the other
// peer gets no document update and the sender gets SyncStatus(0). Before, the
// room kept and stored "hello" and fired OnUpdate, and relayed it to the other
// peer, while the sender was told 0. Once the map's creator is in, the same
// update parks nothing and applies, and reaches the other peer.
func TestInteg_DispatchConfig_RefusedUpdateLeavesTheRoomUnchanged(t *testing.T) {
	const limit, parked = 2, 5
	srv, ts, adapter := dispatchServer(t, limit)

	creator := crdt.New(crdt.WithClientID(1))
	root := creator.GetMap("m")
	creator.Transact(func(txn *crdt.Transaction) { root.Set(txn, "nested", crdt.NewMapPrelim()) })
	created := crdt.EncodeStateAsUpdateV1(creator, nil)
	editor := crdt.New(crdt.WithClientID(2))
	require.NoError(t, crdt.ApplyUpdateV1(editor, created, nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested, ok := v.(*crdt.YMap)
	require.True(t, ok, "nested is %T", v)
	text := editor.GetText("t")
	editor.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "hello", nil)
		for i := range parked {
			nested.Set(txn, "k"+strconv.Itoa(i), i)
		}
	})
	refused := crdt.EncodeStateAsUpdateV1(editor, creator.StateVector())

	sender := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(7)))
	other := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(6)))
	room := srv.GetDoc("room")
	require.NotNil(t, room)
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	var observed atomic.Int32
	room.OnUpdate(func([]byte, any) { observed.Add(1) })
	countSync := func() int {
		n := 0
		for _, tag := range tagsUntilPong(t, other, "room") {
			if tag == tagSync {
				n++
			}
		}
		return n
	}

	hpSend(t, sender, "room", tagSync, ygsync.EncodeUpdate(refused))
	assert.Equal(t, []uint64{0}, syncStatuses(t, sender, "room"), "the sender is told the update was not applied")
	assert.Equal(t, 0, countSync(), "the other peer gets no document update")
	assert.Empty(t, room.GetText("t").ToString(), "the room's text")
	assert.Equal(t, before, crdt.EncodeStateAsUpdateV1(room, nil), "the room's state")
	assert.Zero(t, room.PendingStats().Items, "the room parks nothing of the refused update")
	assert.Equal(t, int32(0), observed.Load(), "the room's OnUpdate observers were handed the refused update")
	assert.Empty(t, adapter.stored(), "the room stored nothing of the refused update")

	hpSend(t, sender, "room", tagSync, ygsync.EncodeUpdate(created))
	hpSend(t, sender, "room", tagSync, ygsync.EncodeUpdate(refused))
	assert.Equal(t, []uint64{1, 1}, syncStatuses(t, sender, "room"))
	assert.Equal(t, 2, countSync(), "the other peer gets the map's creation and the edit")
	assert.Equal(t, "hello", room.GetText("t").ToString())
	assert.Equal(t, int32(2), observed.Load())

	require.Eventually(t, func() bool { return len(adapter.stored()) >= 2 }, 5*time.Second, 5*time.Millisecond)
	stored := adapter.stored()
	assert.Len(t, stored, 2, "the room stored the map's creation and the edit, and nothing of the refused update")
	replay := crdt.New()
	for _, u := range stored {
		require.NoError(t, crdt.ApplyUpdateV1(replay, u, nil))
	}
	assert.Equal(t, "hello", replay.GetText("t").ToString())
	assert.Zero(t, replay.PendingStats().Items, "the stored updates park nothing")
}

// A room under that configuration (dispatchServer) relays and stores exactly
// what an update that fails partway committed (crdt.ApplyUpdateV1Landed). The
// room holds 100 deletions and 40 parked entries. The editor's update inserts
// "abc", which integrates, and sets five entries on a map the room never
// received, which park well inside the cap; its delete set is cut short, so
// the apply fails after them. The update is small enough that no check reads
// it before the apply. The sender gets SyncStatus(0); the other peer gets one
// document update carrying the three characters and nothing more (not the
// parked entries, not the room's deletions), and ends with the room's text;
// the room stores those three characters, once, with no parked entry and no
// deletion it did not already hold (a stored update carries the room's whole
// delete set, as every OnUpdate update does).
func TestInteg_DispatchConfig_PartlyAppliedUpdateRelaysAndStoresWhatLanded(t *testing.T) {
	srv, ts, adapter := dispatchServer(t, 200)
	editorDoc := crdt.New(crdt.WithClientID(7))
	editor := hpDial(t, ts, "room", false, editorDoc)
	watcherDoc := crdt.New(crdt.WithClientID(9))
	watcher := hpDial(t, ts, "room", false, watcherDoc)
	room := srv.GetDoc("room")
	require.NotNil(t, room)

	txt := editorDoc.GetText("t")
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, strings.Repeat("x", 200))))
	before := editorDoc.StateVector()
	editorDoc.Transact(func(txn *crdt.Transaction) {
		for i := range 100 {
			txt.Delete(txn, i, 1)
		}
	})
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(crdt.EncodeStateAsUpdateV1(editorDoc, before)))
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(parkingUpdate(t, 40)))
	require.Equal(t, []uint64{1, 1, 1}, syncStatuses(t, editor, "room"))
	for _, u := range relayedUpdates(t, watcher, "room") {
		require.NoError(t, crdt.ApplyUpdateV1(watcherDoc, u, nil))
	}
	require.Equal(t, 40, room.PendingStats().Items)
	require.Eventually(t, func() bool { return len(adapter.stored()) >= 3 }, 5*time.Second, 5*time.Millisecond)
	storedBefore := len(adapter.stored())

	author := crdt.New(crdt.WithClientID(1))
	root := author.GetMap("m")
	author.Transact(func(txn *crdt.Transaction) { root.Set(txn, "nested", crdt.NewMapPrelim()) })
	require.NoError(t, crdt.ApplyUpdateV1(editorDoc, crdt.EncodeStateAsUpdateV1(author, nil), nil))
	v, _ := editorDoc.GetMap("m").Get("nested")
	nested, ok := v.(*crdt.YMap)
	require.True(t, ok, "nested is %T", v)
	before = editorDoc.StateVector() // includes the author's map, so it is not sent
	editorDoc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "abc", nil)
		for i := range 5 {
			nested.Set(txn, "e"+strconv.Itoa(i), i)
		}
	})
	held, err := crdt.ContentIDsFromUpdateV1(crdt.EncodeStateAsUpdateV1(room, nil))
	require.NoError(t, err)
	cut := crdt.EncodeStateAsUpdateV1(editorDoc, before)
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(cut[:len(cut)-1]))
	require.Equal(t, []uint64{0}, syncStatuses(t, editor, "room"), "the sender is told the update was not applied")
	require.Equal(t, "abc"+strings.Repeat("x", 100), room.GetText("t").ToString(), "the room kept the part that integrated")
	require.Equal(t, 45, room.PendingStats().Items, "the five entries parked")

	relayed := relayedUpdates(t, watcher, "room")
	require.Len(t, relayed, 1, "the other peer gets one document update")
	ids, err := crdt.ContentIDsFromUpdateV1(relayed[0])
	require.NoError(t, err)
	assert.Equal(t, []crdt.ClientID{7}, ids.Inserts.Clients(), "the relay carries only the editor's structs")
	assert.Equal(t, []crdt.IDRange{{Clock: 200, Len: 3}}, ids.Inserts.Ranges(7), `the relay carries "abc" and no parked entry`)
	assert.Empty(t, ids.Deletes.Clients(), "the relay carries no deletion")
	require.NoError(t, crdt.ApplyUpdateV1(watcherDoc, relayed[0], nil))
	assert.Equal(t, room.GetText("t").ToString(), watcherDoc.GetText("t").ToString(), "the other peer ends with the room's text")

	require.Eventually(t, func() bool { return len(adapter.stored()) >= storedBefore+1 }, 5*time.Second, 5*time.Millisecond)
	stored := adapter.stored()
	require.Len(t, stored, storedBefore+1, "the room stores the part once")
	storedIDs, err := crdt.ContentIDsFromUpdateV1(stored[len(stored)-1])
	require.NoError(t, err)
	assert.Equal(t, ids.Inserts.Clients(), storedIDs.Inserts.Clients(), "the room stores what it relayed")
	assert.Equal(t, ids.Inserts.Ranges(7), storedIDs.Inserts.Ranges(7), "the room stores what it relayed")
	assert.Equal(t, held.Deletes.Clients(), storedIDs.Deletes.Clients(), "the room stores no deletion it did not hold")
	assert.Equal(t, held.Deletes.Ranges(7), storedIDs.Deletes.Ranges(7), "the room stores no deletion it did not hold")
	replay := crdt.New()
	for _, u := range stored {
		require.NoError(t, crdt.ApplyUpdateV1(replay, u, nil))
	}
	assert.Equal(t, room.GetText("t").ToString(), replay.GetText("t").ToString(), "the stored updates rebuild the room's text")
}
