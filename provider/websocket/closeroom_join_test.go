package websocket_test

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// blockingFirstStore is a PersistenceAdapter whose first StoreUpdate blocks
// until unblock is called. CloseRoom flushes the room's pending batch after it
// has taken the room's peer list and before it removes the room from the
// server's map, so holding that flush holds CloseRoom inside that window.
type blockingFirstStore struct {
	first   sync.Once
	entered chan struct{}
	release chan struct{}
	unblock func()
}

func newBlockingFirstStore() *blockingFirstStore {
	s := &blockingFirstStore{entered: make(chan struct{}), release: make(chan struct{})}
	s.unblock = sync.OnceFunc(func() { close(s.release) })
	return s
}

func (s *blockingFirstStore) LoadDoc(string) ([]byte, error) { return nil, nil }

func (s *blockingFirstStore) StoreUpdate(string, []byte) error {
	s.first.Do(func() {
		close(s.entered)
		<-s.release
	})
	return nil
}

func insertText(t *testing.T, srv *ygws.Server, text string) {
	t.Helper()
	require.NoError(t, srv.Apply(context.Background(), "room",
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, text, nil) })
		}))
}

// A peer can join a room while CloseRoom closes it: after CloseRoom has taken
// the room's peer list and before it removes the room from the server's map.
// With force, that peer must be disconnected before CloseRoom returns, like
// every other peer of the room. Left connected, it stays on the room CloseRoom
// removed and never receives another update. Without force, the room has a
// peer again, so CloseRoom must leave it open and return ErrRoomHasPeers.
func TestUnit_CloseRoom_PeerJoinsWhileClosing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
	}{
		{name: "force", force: true},
		{name: "no force", force: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newBlockingFirstStore()
			t.Cleanup(store.unblock)
			srv := ygws.NewServerWithPersistence(store)
			// Keep the edit below pending until CloseRoom flushes it.
			srv.PersistCoalesceWindow = time.Minute
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)

			// A room with no peer and an edit that is not stored yet.
			insertText(t, srv, "before")

			closed := make(chan error, 1)
			go func() { closed <- srv.CloseRoom("room", tc.force) }()
			select {
			case <-store.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("CloseRoom did not flush the room's pending edit")
			}

			// CloseRoom is in its flush: it has the room's peer list, and the
			// room is still in the server's map. The server sends the handshake
			// only after the peer is in the room's peers.
			conn := dial(t, ts, "room")
			drainHandshake(t, conn, crdt.New())

			store.unblock()
			var err error
			select {
			case err = <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("CloseRoom did not return")
			}

			if !tc.force {
				require.ErrorIs(t, err, ygws.ErrRoomHasPeers)
				require.NotNil(t, srv.GetDoc("room"), "CloseRoom without force must leave a room that has a peer open")
				// The peer is on the live room: an edit reaches it.
				insertText(t, srv, "after")
				outerType, _ := readOne(t, conn, 2*time.Second)
				assert.Equal(t, uint64(0), outerType, "the peer must receive the edit as a sync message")
				return
			}

			require.NoError(t, err)
			assert.Nil(t, srv.GetDoc("room"), "CloseRoom must remove the room")
			// CloseRoom has returned, so the server has closed the peer's
			// connection already: the read ends with the connection, not at
			// its deadline.
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _, readErr := conn.ReadMessage()
			require.Error(t, readErr)
			var netErr net.Error
			assert.False(t, errors.As(readErr, &netErr) && netErr.Timeout(),
				"the peer that joined while CloseRoom ran is still connected to the room CloseRoom removed")
		})
	}
}
