package websocket_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// framingName names a subtest by the framing it runs on.
func framingName(hocuspocus bool) string {
	if hocuspocus {
		return "hocuspocus"
	}
	return "ywebsocket"
}

// joinFrame encodes one frame in the server's framing: the Hocuspocus one
// prefixes the room name.
func joinFrame(hocuspocus bool, room string, tag uint64, body []byte) []byte {
	if hocuspocus {
		return hpFrame(room, tag, body)
	}
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(tag)
		enc.WriteRaw(body)
	})
}

// joinedSteps sends a Ping (preceded by the joiner's SyncStep1 when
// sendStep1 is set) and reads up to the Pong, applying every sync frame to
// joiner. It returns the full frame of every SyncStep2 on the way.
func joinedSteps(t testing.TB, conn *gws.Conn, hocuspocus bool, room string, joiner *crdt.Doc, sendStep1 bool) [][]byte {
	t.Helper()
	if sendStep1 {
		require.NoError(t, conn.WriteMessage(gws.BinaryMessage,
			joinFrame(hocuspocus, room, tagSync, ygsync.EncodeSyncStep1(joiner))))
	}
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, joinFrame(hocuspocus, room, tagPing, nil)))
	var step2 [][]byte
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := conn.ReadMessage()
		require.NoError(t, err)
		dec := encoding.NewDecoder(data)
		if hocuspocus {
			_, err = dec.ReadVarString()
			require.NoError(t, err)
		}
		tag, err := dec.ReadVarUint()
		require.NoError(t, err)
		switch tag {
		case tagPong:
			return step2
		case tagSync:
			body := dec.RemainingBytes()
			sub, _, err := ygsync.ReadSyncMessage(body)
			require.NoError(t, err)
			if sub == ygsync.MsgSyncStep2 {
				step2 = append(step2, data)
			}
			_, err = ygsync.ApplySyncMessage(joiner, body, nil)
			require.NoError(t, err)
		}
	}
}

// A joiner gets the room's state once, in the SyncStep2 that answers its
// SyncStep1, as y-websocket's and Hocuspocus's servers send it; a peer that
// sends no SyncStep1 gets no state, as on those servers.
func TestInteg_Join_SendsStateOnce(t *testing.T) {
	text := strings.Repeat("x", 10000)
	for _, hocuspocus := range []bool{false, true} {
		t.Run(framingName(hocuspocus), func(t *testing.T) {
			for _, readOnly := range []bool{false, true} {
				t.Run(map[bool]string{false: "readwrite", true: "readonly"}[readOnly], func(t *testing.T) {
					srv := ygws.NewServer()
					srv.HocuspocusFraming = hocuspocus
					srv.Authorize = readOnlyByHeader
					srv.OnLoadDocument = seedText(text)
					ts := httptest.NewServer(srv)
					t.Cleanup(ts.Close)

					joiner := crdt.New()
					conn := dialReadOnly(t, ts, "room", readOnly)
					step2 := joinedSteps(t, conn, hocuspocus, "room", joiner, true)
					want := joinFrame(hocuspocus, "room", tagSync, wholeStep2(t, srv.GetDoc("room")))
					sizes := make([]int, 0, len(step2))
					for _, f := range step2 {
						sizes = append(sizes, len(f))
					}
					require.Equal(t, []int{len(want)}, sizes, "bytes of each SyncStep2 frame the joiner received")
					assert.Equal(t, want, step2[0], "the one SyncStep2 carries the room's full state")
					assert.Equal(t, text, joiner.GetText("t").ToString())

					silent := crdt.New()
					quiet := dialReadOnly(t, ts, "room", readOnly)
					assert.Empty(t, joinedSteps(t, quiet, hocuspocus, "room", silent, false),
						"a peer that sends no SyncStep1 is sent no state")
				})
			}
		})
	}
}

// A client whose token is refused, and which sends it before its SyncStep1
// as @hocuspocus/provider does, receives none of the room's state.
func TestInteg_Join_RejectedTokenGetsNoState(t *testing.T) {
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.OnTokenAuth = func(string, string) (ygws.ConnectionConfig, error) {
			return ygws.ConnectionConfig{}, errors.New("bad token")
		}
		srv.OnLoadDocument = seedText("secret")
	})
	conn := dial(t, ts, "room")
	hpSend(t, conn, "room", tagAuth, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(0) // Token
		enc.WriteVarString("wrong")
	}))
	hpSend(t, conn, "room", tagSync, ygsync.EncodeSyncStep1(crdt.New()))
	var step2 int
	for {
		tag, body, err := hpRead(conn, "room", 2*time.Second)
		if err != nil {
			var closed *gws.CloseError
			require.ErrorAs(t, err, &closed)
			break
		}
		if tag == tagSync {
			if sub, _, err := ygsync.ReadSyncMessage(body); err == nil && sub == ygsync.MsgSyncStep2 {
				step2++
			}
		}
	}
	assert.Zero(t, step2, "SyncStep2 frames the rejected client received")
}

// connectTags sends a Ping and returns the outer tag of every frame the
// server sent before the Pong.
func connectTags(t *testing.T, conn *gws.Conn, hocuspocus bool, room string) []uint64 {
	t.Helper()
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, joinFrame(hocuspocus, room, tagPing, nil)))
	var tags []uint64
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := conn.ReadMessage()
		require.NoError(t, err)
		dec := encoding.NewDecoder(data)
		if hocuspocus {
			_, err = dec.ReadVarString()
			require.NoError(t, err)
		}
		tag, err := dec.ReadVarUint()
		require.NoError(t, err)
		if tag == tagPong {
			return tags
		}
		tags = append(tags, tag)
	}
}

// On connect a peer gets SyncStep1, and awareness only when the room holds
// awareness states, as y-websocket's and Hocuspocus's servers send them.
func TestInteg_Join_AwarenessOnlyWhenTheRoomHasStates(t *testing.T) {
	for _, hocuspocus := range []bool{false, true} {
		t.Run(framingName(hocuspocus), func(t *testing.T) {
			srv := ygws.NewServer()
			srv.HocuspocusFraming = hocuspocus
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)

			first := dial(t, ts, "room")
			assert.Equal(t, []uint64{tagSync}, connectTags(t, first, hocuspocus, "room"),
				"a room without awareness states sends only SyncStep1")

			present := awareness.New(42)
			present.SetLocalState(map[string]any{"name": "a"})
			require.NoError(t, first.WriteMessage(gws.BinaryMessage, joinFrame(hocuspocus, "room", tagAwareness,
				encoding.EncodeBytes(func(enc *encoding.Encoder) { enc.WriteVarBytes(present.EncodeUpdate(nil)) }))))
			connectTags(t, first, hocuspocus, "room") // the awareness is applied before the Pong

			second := dial(t, ts, "room")
			assert.Equal(t, []uint64{tagSync, tagAwareness}, connectTags(t, second, hocuspocus, "room"),
				"a room with an awareness state sends SyncStep1, then awareness")
		})
	}
}

// seedText returns an OnLoadDocument hook that fills YText "t" with text.
func seedText(text string) func(context.Context, string, *crdt.Doc) error {
	return func(_ context.Context, _ string, doc *crdt.Doc) error {
		txt := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, text, nil) })
		return nil
	}
}
