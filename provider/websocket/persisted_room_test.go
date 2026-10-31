package websocket_test

import (
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// storeLog is a PersistenceAdapter that loads nothing and keeps every update
// the room stores, in order.
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

// persistingServer starts a Hocuspocus-framed server with a pending cap of
// limit and returns its store.
func persistingServer(t *testing.T, limit int) (*ygws.Server, *httptest.Server, *storeLog) {
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

// A persisting room refuses an update that would park past the cap without
// storing, relaying or observing it, and applies it once its parent is in.
func TestInteg_PersistedRoom_RefusedUpdateLeavesTheRoomUnchanged(t *testing.T) {
	const limit, parked = 2, 5
	srv, ts, adapter := persistingServer(t, limit)

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
