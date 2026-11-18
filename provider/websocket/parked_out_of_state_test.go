package websocket_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// A room whose document keeps its parked content out of its state
// (crdt.Doc.SetParkedOutOfState) answers a joiner's SyncStep1 without what it
// parks; off, the answer carries it.
func TestInteg_ParkedOutOfState_TheStateAnswerCarriesNothingParked(t *testing.T) {
	for name, on := range map[string]bool{"on": true, "off": false} {
		t.Run(name, func(t *testing.T) {
			srv, ts := hocuspocusServer(t, func(srv *ygws.Server) {
				srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
					doc.SetParkedOutOfState(on)
					return nil
				}
			})
			editor := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(8)))

			// Client 7's "ef" waits for its "cd", which the room never gets.
			writer := crdt.New(crdt.WithClientID(7))
			ab := textUpdate(writer, "ab")
			textUpdate(writer, "cd")
			late := textUpdate(writer, "ef")
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(ab))
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(late))
			require.Len(t, syncStatuses(t, editor, "room"), 2)
			room := srv.GetDoc("room")
			require.Equal(t, 1, room.PendingStats().Items, "the room parks the late frame")

			watcherDoc := crdt.New(crdt.WithClientID(9))
			watcher := hpDial(t, ts, "room", false, watcherDoc)
			watcherSyncFrames(t, watcher, watcherDoc)
			parked := 0
			if !on {
				parked = 1
			}
			assert.Equal(t, parked, watcherDoc.PendingStats().Items, "what the answer carried parked")
			assert.Equal(t, "ab", watcherDoc.GetText("t").ToString())
		})
	}
}
