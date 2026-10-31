package websocket_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// keepingStore is a persistence adapter that keeps each update the server
// stores.
type keepingStore struct {
	mu      sync.Mutex
	updates [][]byte
}

func (s *keepingStore) LoadDoc(string) ([]byte, error) { return nil, nil }

func (s *keepingStore) StoreUpdate(_ string, update []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, update)
	return nil
}

func (s *keepingStore) stored() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.updates)
}

// noOpSetup is a room setup under test: without persistence, with a store
// that keeps one write per update, or with an OnPeerUpdate hook that applies.
type noOpSetup struct {
	name  string
	store *keepingStore
	hook  bool
}

func noOpSetups() []noOpSetup {
	return []noOpSetup{{name: "no persistence"}, {name: "persistence", store: &keepingStore{}}, {name: "hook", hook: true}}
}

func startNoOpServer(t *testing.T, setup noOpSetup) (*ygws.Server, *httptest.Server) {
	t.Helper()
	srv := ygws.NewServer()
	if setup.store != nil {
		srv = ygws.NewServerWithPersistence(setup.store)
		srv.PersistCoalesceWindow = -1
	}
	if setup.hook {
		srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error { return apply() }
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, ts
}

// nextRelayedUpdate reads the next frame conn gets and returns the update it
// carries.
func nextRelayedUpdate(t *testing.T, conn *gws.Conn) []byte {
	t.Helper()
	outer, payload := readOne(t, conn, 2*time.Second)
	require.Equal(t, uint64(0), outer, "a sync frame")
	sub, update, err := ygsync.ReadSyncMessage(payload)
	require.NoError(t, err)
	require.Equal(t, ygsync.MsgUpdate, sub)
	return update
}

// keystrokes returns n updates, each one character typed after the last.
func keystrokes(n int) [][]byte {
	doc := crdt.New(crdt.WithClientID(7))
	txt := doc.GetText("t")
	var out [][]byte
	unsub := doc.OnUpdate(func(u []byte, _ any) { out = append(out, u) })
	defer unsub()
	for i := range n {
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, i, "x", nil) })
	}
	return out
}

func TestInteg_ResentUpdateReachesNoPeerAndNoStore(t *testing.T) {
	for _, tc := range noOpSetups() {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := startNoOpServer(t, tc)
			sender, receiver := dial(t, ts, "room"), dial(t, ts, "room")
			drainHandshake(t, sender, crdt.New())
			drainHandshake(t, receiver, crdt.New())
			u := keystrokes(2)

			sendUpdate(t, sender, u[0])
			assert.Equal(t, u[0], nextRelayedUpdate(t, receiver))
			sendUpdate(t, sender, u[0])
			sendUpdate(t, sender, u[1])
			assert.Equal(t, u[1], nextRelayedUpdate(t, receiver), "the re-sent update is not relayed")

			if tc.store != nil {
				// The store writes in order, so the re-sent update would be
				// stored before the second one.
				require.Eventually(t, func() bool { return tc.store.stored() >= 2 }, 2*time.Second, 5*time.Millisecond)
				assert.Equal(t, 2, tc.store.stored(), "the re-sent update is not stored")
			}
		})
	}
}

func TestInteg_ParkedUpdateStillReachesPeers(t *testing.T) {
	for _, tc := range noOpSetups() {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := startNoOpServer(t, tc)
			sender, receiver := dial(t, ts, "room"), dial(t, ts, "room")
			drainHandshake(t, sender, crdt.New())
			drainHandshake(t, receiver, crdt.New())
			u := keystrokes(2)

			sendUpdate(t, sender, u[1])
			assert.Equal(t, u[1], nextRelayedUpdate(t, receiver), "an update the room parks is relayed")
			assert.Equal(t, 1, srv.GetDoc("room").PendingStats().Items)
		})
	}
}

func TestInteg_InjectedHeldUpdateReachesNoPeer(t *testing.T) {
	for _, tc := range noOpSetups() {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := startNoOpServer(t, tc)
			receiver := dial(t, ts, "room")
			drainHandshake(t, receiver, crdt.New())
			u := keystrokes(2)
			inject := func(update []byte) {
				require.NoError(t, srv.Inject(context.Background(), cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: update}))
			}

			inject(u[0])
			assert.Equal(t, u[0], nextRelayedUpdate(t, receiver))
			inject(u[0])
			inject(u[1])
			assert.Equal(t, u[1], nextRelayedUpdate(t, receiver), "the held update is not relayed")
		})
	}
}

func TestUnit_Apply_TransactThatChangesNothing_ErrNoChanges(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.Apply(context.Background(), "room", func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(*crdt.Transaction) {})
	})
	assert.ErrorIs(t, err, ygws.ErrNoChanges)
}
