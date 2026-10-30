package websocket_test

import (
	"context"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// applySync applies one sync message the server sent to doc.
func applySync(t *testing.T, doc *crdt.Doc, payload []byte) {
	t.Helper()
	_, err := ygsync.ApplySyncMessage(doc, payload, nil)
	require.NoError(t, err)
}

// truncatedEdit returns "hello" written by client 1 and that client's next
// edit ("!" at 5, delete 1..3) with its last byte cut off: its struct
// integrates, then its delete set does not decode.
func truncatedEdit() (seed, edit []byte) {
	doc := crdt.New(crdt.WithClientID(1))
	text := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "hello", nil) })
	seed = crdt.EncodeStateAsUpdateV1(doc, nil)
	sv := doc.StateVector()
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 5, "!", nil)
		text.Delete(txn, 1, 2)
	})
	whole := crdt.EncodeStateAsUpdateV1(doc, sv)
	return seed, whole[:len(whole)-1]
}

// overflowingEdit returns client 2's update that writes "hello" and sets n
// keys on a map client 3 created, which a node that never got the map parks.
func overflowingEdit(t *testing.T, n int) []byte {
	t.Helper()
	creator := crdt.New(crdt.WithClientID(3))
	root := creator.GetMap("m")
	creator.Transact(func(txn *crdt.Transaction) { root.Set(txn, "nested", crdt.NewMapPrelim()) })
	editor := crdt.New(crdt.WithClientID(2))
	require.NoError(t, crdt.ApplyUpdateV1(editor, crdt.EncodeStateAsUpdateV1(creator, nil), nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested, ok := v.(*crdt.YMap)
	require.True(t, ok, "nested is %T", v)
	text := editor.GetText("t")
	editor.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "hello", nil)
		for i := range n {
			nested.Set(txn, "k"+strconv.Itoa(i), i)
		}
	})
	return crdt.EncodeStateAsUpdateV1(editor, creator.StateVector())
}

// A relay-injected update that fails after part of it integrated leaves that
// part in this node's room and tells the room's update observers, persistence
// among them. The node's locally connected peers get exactly that part too,
// as a peer's partly applied update reaches the room's other peers (#291).
// Before this, Inject returned the error and relayed nothing, so the peer
// held less than the room. An update that integrates nothing, or one whose
// landed part a redelivery already brought, relays nothing: the next frame
// the peer reads is a later, whole update's.
func TestInteg_Inject_PartlyAppliedUpdateReachesLocalPeers(t *testing.T) {
	seed, truncated := truncatedEdit()
	cases := []struct {
		name       string
		maxPending int
		seed       []byte
		update     []byte
		roomText   string
		relays     bool
	}{
		{name: "a delete set that does not decode", seed: seed, update: truncated, roomText: "hello!", relays: true},
		{name: "the pending cap refusing a deferred item", maxPending: 3, update: overflowingEdit(t, 7), roomText: "hello", relays: true},
		{name: "nothing integrated", update: []byte{0x01, 0x05}, relays: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			relay := cluster.NewMemRelay()
			defer func() { require.NoError(t, relay.Close()) }()
			srv := ygws.NewServer()
			srv.MaxPendingItems = tc.maxPending
			require.NoError(t, srv.AttachRelay(relay))
			ts := httptest.NewServer(srv)
			defer ts.Close()

			peerDoc := crdt.New(crdt.WithClientID(7))
			conn := dial(t, ts, "room")
			drainHandshake(t, conn, peerDoc)
			ctx := context.Background()
			if tc.seed != nil {
				require.NoError(t, srv.Inject(ctx, cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: tc.seed}))
				readUntil(t, conn, peerDoc, 2*time.Second,
					func() bool { return peerDoc.GetText("t").ToString() == "hello" }, "the peer gets the seed")
			}
			room := srv.GetDoc("room")
			require.NotNil(t, room)
			var observed atomic.Int32
			room.OnUpdate(func([]byte, any) { observed.Add(1) })

			err := srv.Inject(ctx, cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: tc.update})
			require.ErrorIs(t, err, crdt.ErrInvalidUpdate, "Inject still reports the apply's failure")
			require.Equal(t, tc.roomText, room.GetText("t").ToString())

			if tc.relays {
				require.Equal(t, int32(1), observed.Load(), "the room's update observers are told of the part that landed")
				outer, payload := readOne(t, conn, 2*time.Second)
				require.Equal(t, uint64(0), outer, "the local peer gets what the room committed")
				applySync(t, peerDoc, payload)
				require.Equal(t, crdt.EncodeStateAsUpdateV1(room, nil), crdt.EncodeStateAsUpdateV1(peerDoc, nil),
					"the local peer holds what the room holds")

				// Redelivered, the update integrates nothing new: it fails
				// again and relays nothing more.
				err = srv.Inject(ctx, cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: tc.update})
				require.ErrorIs(t, err, crdt.ErrInvalidUpdate)
			}

			// A later, whole update from another writer is the next frame the
			// peer reads, so nothing was relayed in between.
			later := crdt.New(crdt.WithClientID(5))
			lt := later.GetText("u")
			later.Transact(func(txn *crdt.Transaction) { lt.Insert(txn, 0, "later", nil) })
			require.NoError(t, srv.Inject(ctx, cluster.Inbound{Room: "room", Kind: cluster.KindSync,
				Data: crdt.EncodeStateAsUpdateV1(later, nil)}))
			outer, payload := readOne(t, conn, 2*time.Second)
			require.Equal(t, uint64(0), outer)
			applySync(t, peerDoc, payload)
			require.Equal(t, "later", peerDoc.GetText("u").ToString(), "the next frame is the later update's")
			require.Equal(t, crdt.EncodeStateAsUpdateV1(room, nil), crdt.EncodeStateAsUpdateV1(peerDoc, nil))
		})
	}
}
