package websocket_test

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// nextSync reads frames from conn until a sync frame (outer type 0) arrives
// or within passes, and returns its payload and whether one arrived.
func nextSync(conn *gws.Conn, within time.Duration) ([]byte, bool) {
	deadline := time.Now().Add(within)
	for {
		_ = conn.SetReadDeadline(deadline)
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil, false
		}
		dec := encoding.NewDecoder(data)
		outer, err := dec.ReadVarUint()
		if err == nil && outer == 0 {
			return dec.RemainingBytes(), true
		}
	}
}

// A relay-injected update that fails after part of it integrated - here a
// real edit with its last byte cut off, so its struct ("!") integrates and
// then its delete set does not decode - leaves that part in this node's room
// and tells the room's update observers, persistence among them. The node's
// locally connected peers must get exactly that part too, as a peer's partly
// applied update reaches the room's other peers (#291). On 64fa0b47 Inject
// returned the error and relayed nothing, so the peer held "hello" while the
// room held "hello!".
func TestInteg_Inject_PartlyAppliedUpdateReachesLocalPeers(t *testing.T) {
	relay := cluster.NewMemRelay()
	defer func() { require.NoError(t, relay.Close()) }()
	srv := ygws.NewServer()
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	peerDoc := crdt.New(crdt.WithClientID(7))
	conn := dial(t, ts, "room")
	drainHandshake(t, conn, peerDoc)

	remote := crdt.New(crdt.WithClientID(1))
	text := remote.GetText("t")
	remote.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "hello", nil) })
	seed := crdt.EncodeStateAsUpdateV1(remote, nil)
	sv := remote.StateVector()
	remote.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 5, "!", nil)
		text.Delete(txn, 1, 2)
	})
	whole := crdt.EncodeStateAsUpdateV1(remote, sv)
	truncated := whole[:len(whole)-1]

	ctx := context.Background()
	require.NoError(t, srv.Inject(ctx, cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: seed}))
	payload, ok := nextSync(conn, 2*time.Second)
	require.True(t, ok, "the peer gets the seed")
	_, err := ygsync.ApplySyncMessage(peerDoc, payload, nil)
	require.NoError(t, err)
	require.Equal(t, "hello", peerDoc.GetText("t").ToString())

	room := srv.GetDoc("room")
	require.NotNil(t, room)
	var observed atomic.Int32
	room.OnUpdate(func([]byte, any) { observed.Add(1) })

	injectErr := srv.Inject(ctx, cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: truncated})
	require.Equal(t, "hello!", room.GetText("t").ToString(), "the room keeps the struct that integrated")
	require.Equal(t, int32(1), observed.Load(), "the room's update observers are told of it")

	payload, ok = nextSync(conn, 2*time.Second)
	require.True(t, ok, "the local peer gets what the room committed")
	_, err = ygsync.ApplySyncMessage(peerDoc, payload, nil)
	require.NoError(t, err)
	require.Equal(t, "hello!", peerDoc.GetText("t").ToString())
	require.Equal(t, crdt.EncodeStateAsUpdateV1(room, nil), crdt.EncodeStateAsUpdateV1(peerDoc, nil),
		"the local peer holds what the room holds")
	require.ErrorIs(t, injectErr, crdt.ErrInvalidUpdate, "Inject still reports the apply's failure")
}
