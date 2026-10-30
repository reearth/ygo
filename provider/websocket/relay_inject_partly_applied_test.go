package websocket_test

import (
	"bytes"
	"context"
	"net/http/httptest"
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

// landedPart returns the content IDs of the part of update that a document
// holding only seed integrates before the apply fails.
func landedPart(t *testing.T, seed, update []byte) crdt.ContentIDs {
	t.Helper()
	doc := crdt.New()
	if seed != nil {
		require.NoError(t, crdt.ApplyUpdateV1(doc, seed, nil))
	}
	landed, err := crdt.ApplyUpdateV1Landed(doc, update, nil)
	require.Error(t, err)
	require.NotNil(t, landed)
	return contentIDs(t, landed)
}

// contentIDs returns the structs and deletions a V1 update carries.
func contentIDs(t *testing.T, update []byte) crdt.ContentIDs {
	t.Helper()
	ids, err := crdt.ContentIDsFromUpdateV1(update)
	require.NoError(t, err)
	return ids
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

// invalidStringEdit returns client 2's update holding two structs, "hello"
// and then "xyz" inserted before it, with the second's bytes replaced by
// ones that are not UTF-8: the first struct integrates, then the second does
// not decode. No check before the apply reads a struct's string, so the
// apply alone refuses it, at that struct.
func invalidStringEdit(t *testing.T) []byte {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(2))
	text := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "hello", nil) })
	doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "xyz", nil) })
	update := crdt.EncodeStateAsUpdateV1(doc, nil)
	require.Equal(t, 1, bytes.Count(update, []byte("xyz")))
	return bytes.Replace(update, []byte("xyz"), []byte{0xff, 0xfe, 0xfd}, 1)
}

// A relay-injected update that fails after part of it integrated leaves that
// part in this node's room and tells the room's update observers, so the
// node's locally connected peers must get exactly that part too. An update
// that integrates nothing, or one whose landed part a redelivery already
// brought, relays nothing: the next frame the peer reads is a later, whole
// update's.
func TestInteg_Inject_PartlyAppliedUpdateReachesLocalPeers(t *testing.T) {
	seed, truncated := truncatedEdit()
	cases := []struct {
		name     string
		seed     []byte
		update   []byte
		roomText string
		relays   bool
	}{
		{name: "a delete set that does not decode", seed: seed, update: truncated, roomText: "hello!", relays: true},
		{name: "a later struct that does not decode", update: invalidStringEdit(t), roomText: "hello", relays: true},
		{name: "nothing integrated", update: []byte{0x01, 0x05}, relays: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			relay := cluster.NewMemRelay()
			defer func() { require.NoError(t, relay.Close()) }()
			srv := ygws.NewServer()
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

			// Inject reports the apply's failure, and never a broadcast of an
			// empty part.
			injectFails := func() {
				t.Helper()
				err := srv.Inject(ctx, cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: tc.update})
				require.ErrorIs(t, err, crdt.ErrInvalidUpdate)
				require.NotErrorIs(t, err, ygws.ErrInvalidUpdate)
			}
			injectFails()
			require.Equal(t, tc.roomText, room.GetText("t").ToString())

			if tc.relays {
				require.Equal(t, int32(1), observed.Load(), "the room's update observers are told of the part that landed")
				outer, payload := readOne(t, conn, 2*time.Second)
				require.Equal(t, uint64(0), outer, "the local peer gets what the room committed")
				_, got, err := ygsync.ReadSyncMessage(payload)
				require.NoError(t, err)
				require.Equal(t, landedPart(t, tc.seed, tc.update), contentIDs(t, got),
					"the frame carries the landed part and nothing else")
				applySync(t, peerDoc, payload)
				require.Equal(t, crdt.EncodeStateAsUpdateV1(room, nil), crdt.EncodeStateAsUpdateV1(peerDoc, nil),
					"the local peer holds what the room holds")

				// Redelivered, the update integrates nothing new: it fails
				// again and relays nothing more.
				injectFails()
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
