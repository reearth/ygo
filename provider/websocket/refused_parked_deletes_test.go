package websocket_test

import (
	"net/http/httptest"
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

// recordingAdapter is a PersistenceAdapter that loads no stored state and
// keeps every update the room stores, in order.
type recordingAdapter struct {
	mu      sync.Mutex
	updates [][]byte
}

func (a *recordingAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }

func (a *recordingAdapter) StoreUpdate(_ string, update []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, append([]byte(nil), update...))
	return nil
}

func (a *recordingAdapter) stored() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]byte(nil), a.updates...)
}

// missingDeleteRanges is a V1 update of no structs whose delete set names n
// one-clock ranges of client, every other clock from 0.
func missingDeleteRanges(client crdt.ClientID, n int) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(0) // clients
		enc.WriteVarUint(1) // delete set: one client
		enc.WriteVarUint(uint64(client))
		enc.WriteVarUint(uint64(n))
		for i := range n {
			enc.WriteVarUint(uint64(2 * i))
			enc.WriteVarUint(1)
		}
	})
}

// documentUpdates sends a Ping on conn and reads frames until its Pong,
// counting the document updates (Sync frames) on the way.
func documentUpdates(t *testing.T, conn *gws.Conn, room string) int {
	t.Helper()
	hpSend(t, conn, room, tagPing, nil)
	n := 0
	for {
		tag, _, err := hpRead(conn, room, 2*time.Second)
		require.NoError(t, err)
		switch tag {
		case tagPong:
			return n
		case tagSync:
			n++
		}
	}
}

// A refused over-cap update must leave the room unchanged: nothing applied,
// stored or sent to the other peer, and SyncStatus(0) to the sender.
func TestInteg_RefusedParkedDeleteUpdateLeavesTheRoomUnchanged(t *testing.T) {
	const limit = 10
	adapter := &recordingAdapter{}
	srv := ygws.NewServerWithPersistence(adapter)
	srv.HocuspocusFraming = true
	srv.PersistCoalesceWindow = -1
	srv.MaxPendingItems = limit
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	senderDoc := crdt.New(crdt.WithClientID(5))
	sender := hpDial(t, ts, "room", false, senderDoc)
	other := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(6)))

	hpSend(t, sender, "room", tagSync, ygsync.EncodeUpdate(textUpdate(senderDoc, "hello")))
	require.Equal(t, []uint64{1}, syncStatuses(t, sender, "room"))
	require.Equal(t, 1, documentUpdates(t, other, "room"), "the other peer gets the first edit")
	require.Eventually(t, func() bool { return len(adapter.stored()) == 1 }, 5*time.Second, 5*time.Millisecond)

	sv := senderDoc.StateVector()
	text := senderDoc.GetText("t")
	senderDoc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 5, "!", nil)
		text.Delete(txn, 1, 2)
	})
	edit := crdt.EncodeStateAsUpdateV1(senderDoc, sv)
	refused, err := crdt.MergeUpdatesV1(edit, missingDeleteRanges(98, limit+1))
	require.NoError(t, err)

	room := srv.GetDoc("room")
	require.NotNil(t, room)
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	var observed atomic.Int32
	room.OnUpdate(func([]byte, any) { observed.Add(1) })

	hpSend(t, sender, "room", tagSync, ygsync.EncodeUpdate(refused))
	assert.Equal(t, []uint64{0}, syncStatuses(t, sender, "room"), "the sender is told the update was not applied")
	assert.Equal(t, 0, documentUpdates(t, other, "room"), "the other peer gets no document update")
	assert.Equal(t, "hello", room.GetText("t").ToString(), "the room's text")
	assert.Equal(t, before, crdt.EncodeStateAsUpdateV1(room, nil), "the room's state")
	assert.Equal(t, int32(0), observed.Load(), "the room's OnUpdate observers were handed the refused update")

	hpSend(t, sender, "room", tagSync, ygsync.EncodeUpdate(edit))
	assert.Equal(t, []uint64{1}, syncStatuses(t, sender, "room"))
	assert.Equal(t, 1, documentUpdates(t, other, "room"), "the other peer gets the edit alone")
	assert.Equal(t, "hlo!", room.GetText("t").ToString())
	assert.Equal(t, int32(1), observed.Load())

	require.Eventually(t, func() bool { return len(adapter.stored()) >= 2 }, 5*time.Second, 5*time.Millisecond)
	stored := adapter.stored()
	assert.Len(t, stored, 2, "the room stored the first edit and the edit alone, and nothing of the refused update")
	replay := crdt.New()
	for _, u := range stored {
		require.NoError(t, crdt.ApplyUpdateV1(replay, u, nil))
	}
	assert.Equal(t, "hlo!", replay.GetText("t").ToString(), "the stored updates replay to the room's text")
	assert.Zero(t, replay.PendingStats().DeleteRanges, "the stored updates park no delete range")
}
