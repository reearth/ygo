package websocket_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
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

var errCheckRefused = errors.New("the room's check refuses it")

// checkedRoom is a room whose document refuses, by its apply check, any state
// whose text "t" holds EVIL or BAD, and which holds, parked, client 4's EVIL:
// written after client 5's first character, which the room does not hold yet.
// first is that character, the update that fills EVIL's gap; bad is client
// 5's next update, which writes BAD. The editor connection sends updates, and
// the watcher connection is another peer of the room, which has read
// everything the room sent it so far.
type checkedRoom struct {
	srv             *ygws.Server
	room            *crdt.Doc
	editor, watcher *gws.Conn
	first, bad      []byte
}

// pathOf sends update to the room the way path names: from the editor, from
// the editor through an OnPeerUpdate hook that returns apply's error, or from
// the cluster relay (Server.Inject). It returns the statuses the editor is
// answered with, and the error Inject returns.
type pathOf func(t *testing.T, r *checkedRoom, update []byte) ([]uint64, error)

func fromPeer(t *testing.T, r *checkedRoom, update []byte) ([]uint64, error) {
	hpSend(t, r.editor, "room", tagSync, ygsync.EncodeUpdate(update))
	return syncStatuses(t, r.editor, "room"), nil
}

func fromCluster(_ *testing.T, r *checkedRoom, update []byte) ([]uint64, error) {
	return nil, r.srv.Inject(context.Background(), cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: update})
}

var applyCheckPaths = []struct {
	name string
	// hook is whether the server has an OnPeerUpdate hook, and relay whether
	// the update comes from the cluster relay rather than a peer, which is
	// answered no SyncStatus.
	hook, relay bool
	send        pathOf
}{
	{"peer", false, false, fromPeer},
	{"peer through OnPeerUpdate", true, false, fromPeer},
	{"cluster relay", false, true, fromCluster},
}

func newCheckedRoom(t *testing.T, hook bool) *checkedRoom {
	t.Helper()
	honest := crdt.New(crdt.WithClientID(5))
	text := honest.GetText("t")
	typed := func(s string) []byte {
		sv := honest.StateVector()
		honest.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), s, nil) })
		return crdt.EncodeStateAsUpdateV1(honest, sv)
	}
	r := &checkedRoom{first: typed("h")}
	attacker := crdt.New(crdt.WithClientID(4))
	require.NoError(t, crdt.ApplyUpdateV1(attacker, r.first, nil))
	sv := attacker.StateVector()
	evilText := attacker.GetText("t")
	attacker.Transact(func(txn *crdt.Transaction) { evilText.Insert(txn, evilText.Len(), "EVIL", nil) })
	evil := crdt.EncodeStateAsUpdateV1(attacker, sv)
	r.bad = typed("BAD")

	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
			text := doc.GetText("t")
			doc.SetApplyCheck(func(_ any, v *crdt.ApplyView) error {
				if s := v.TextString(text); strings.Contains(s, "EVIL") || strings.Contains(s, "BAD") {
					return errCheckRefused
				}
				return nil
			})
			return nil
		}
		if hook {
			srv.OnPeerUpdate = func(_ ygws.PeerUpdateInfo, apply func() error) error { return apply() }
		}
	})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	r.srv = srv
	r.editor = hpDial(t, ts, "room", false, crdt.New())
	r.room = srv.GetDoc("room")
	require.NotNil(t, r.room)
	// EVIL parks in the room's document; parking changes nothing a peer is
	// sent, and the check is not called for it.
	require.NoError(t, crdt.ApplyUpdateV1(r.room, evil, nil))
	require.Equal(t, 1, r.room.PendingStats().Items, "EVIL parks behind client 5's first character")
	r.watcher = hpDial(t, ts, "room", false, crdt.New())
	syncBodies(t, r.watcher)
	return r
}

// syncBodies reads conn's frames up to the Pong of a Ping it sends and
// returns the bodies of its sync frames, applying none of them.
func syncBodies(t *testing.T, conn *gws.Conn) [][]byte {
	t.Helper()
	hpSend(t, conn, "room", tagPing, nil)
	var bodies [][]byte
	for {
		tag, body, err := hpRead(conn, "room", 2*time.Second)
		require.NoError(t, err)
		switch tag {
		case tagPong:
			return bodies
		case tagSync:
			bodies = append(bodies, body)
		}
	}
}

// An update whose drain the room's apply check refuses stood: the room took
// it and rolled back only the drain of what another client parked. The sender
// is answered SyncStatus(1), as for any applied update, so its client does
// not hold the update as unsent; the update is relayed to the room's other
// peers; and the drain's parked content is neither applied nor relayed. The
// same holds through an OnPeerUpdate hook that returns apply's error and for
// an update the cluster relay delivers.
func TestInteg_ApplyCheck_AnUpdateWhoseDrainIsRefusedIsAnsweredAppliedAndRelayed(t *testing.T) {
	for _, path := range applyCheckPaths {
		t.Run(path.name, func(t *testing.T) {
			r := newCheckedRoom(t, path.hook)
			statuses, err := path.send(t, r, r.first)
			require.NoError(t, err, "the stood update was reported refused")
			if !path.relay {
				assert.Equal(t, []uint64{1}, statuses, "the stood update is answered SyncStatus(1)")
			}

			bodies := syncBodies(t, r.watcher)
			relayed := false
			for _, body := range bodies {
				relayed = relayed || bytes.Contains(body, r.first)
				assert.NotContains(t, string(body), "EVIL", "the drain's parked content was relayed")
			}
			assert.True(t, relayed, "the stood update reached no other peer (%d sync frames)", len(bodies))
			assert.Equal(t, "h", r.room.GetText("t").ToString(), "the room holds the update and nothing the drain landed")
			assert.Equal(t, 1, r.room.PendingStats().Items, "EVIL is parked again")
		})
	}
}

// An update the room's apply check refuses is refused whole: the sender is
// answered SyncStatus(0), it reaches no other peer, and the room is as it
// was. Through an OnPeerUpdate hook that returns apply's error the peer is
// answered the same, and the cluster relay is returned the refusal.
func TestInteg_ApplyCheck_AnUpdateTheCheckRefusesIsAnsweredZeroAndRelayedToNobody(t *testing.T) {
	for _, path := range applyCheckPaths {
		t.Run(path.name, func(t *testing.T) {
			r := newCheckedRoom(t, path.hook)
			_, err := path.send(t, r, r.first)
			require.NoError(t, err)
			syncBodies(t, r.watcher)
			before := crdt.EncodeStateAsUpdateV1(r.room, nil)

			statuses, err := path.send(t, r, r.bad)
			if path.relay {
				var refused *crdt.RefusedUpdateError
				require.ErrorAs(t, err, &refused, "the relay was not told of the refusal")
				require.ErrorIs(t, err, crdt.ErrApplyCheck)
			} else {
				require.NoError(t, err)
				assert.Equal(t, []uint64{0}, statuses, "the refused update is answered SyncStatus(0)")
			}
			assert.Empty(t, syncBodies(t, r.watcher), "the refused update reached another peer")
			assert.Equal(t, before, crdt.EncodeStateAsUpdateV1(r.room, nil), "the room's document changed")
		})
	}
}
