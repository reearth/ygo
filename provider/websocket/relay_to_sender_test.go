package websocket_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// Under RelayIntegratedOnly the frame that fills another peer's gap is
// relayed, with what its drain landed, to every peer but its sender, so the
// sender never receives the parked content its own frame landed; without the
// option it received the parked frame as sent and integrated it itself.
func TestAudit_RelayIntegratedOnly_TheFillerNeverReceivesWhatItsFillLanded(t *testing.T) {
	for name, relayIntegratedOnly := range map[string]bool{"on": true, "off": false} {
		t.Run(name, func(t *testing.T) {
			srv, ts := hocuspocusServer(t, func(srv *ygws.Server) { srv.RelayIntegratedOnly = relayIntegratedOnly })
			aDoc := crdt.New(crdt.WithClientID(8))
			a := hpDial(t, ts, "room", false, aDoc)
			bDoc := crdt.New(crdt.WithClientID(9))
			b := hpDial(t, ts, "room", false, bDoc)

			// B types "ab"; A holds it already (an earlier room instance relayed
			// it and never stored it) and types "cd" after it.
			x := textUpdate(bDoc, "ab")
			require.NoError(t, crdt.ApplyUpdateV1(aDoc, x, nil))
			sv := aDoc.StateVector()
			txt := aDoc.GetText("t")
			aDoc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 2, "cd", nil) })
			s := crdt.EncodeStateAsUpdateV1(aDoc, sv)

			hpSend(t, a, "room", tagSync, ygsync.EncodeUpdate(s))
			require.Equal(t, []uint64{1}, syncStatuses(t, a, "room"))
			require.Equal(t, 1, srv.GetDoc("room").PendingStats().Items, "A's frame parks behind B's")
			watcherSyncFrames(t, b, bDoc)

			hpSend(t, b, "room", tagSync, ygsync.EncodeUpdate(x))
			// Read every frame B is sent up to a Pong, applying each sync frame
			// (syncStatuses would drop them).
			bFrames := watcherSyncFrames(t, b, bDoc)
			require.Equal(t, "abcd", srv.GetDoc("room").GetText("t").ToString(), "B's frame lands A's")
			t.Logf("B was sent %d sync frames after its fill", len(bFrames))
			watcherSyncFrames(t, a, aDoc)
			t.Logf("relay %s: room %q, A %q, B %q", name, srv.GetDoc("room").GetText("t").ToString(),
				aDoc.GetText("t").ToString(), bDoc.GetText("t").ToString())
			assert.Equal(t, "abcd", bDoc.GetText("t").ToString(), "B holds what the room holds")
		})
	}
}
