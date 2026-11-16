package websocket_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
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

// Tests for the frames a peer's SyncStep1 is answered with. The room's state
// goes out as Update messages, each of which integrates on its own, and then
// one SyncStep2, last, so that a receiver applies the state one frame per task
// and a client that reads a SyncStep2 as the end of its sync reads it once
// everything is there.

// answerFrame is one frame a client read, decoded far enough to tell what it
// is and measured as it crossed the socket.
type answerFrame struct {
	tag   uint64
	sub   int // the sync sub-type, -1 for a frame that is not sync
	body  []byte
	bytes int
}

// readAnswer reads the sync frames of one answer, up to and including its
// SyncStep2, and returns the bytes of sync message they carried between them.
// Frames of other kinds are skipped.
func readAnswer(t *testing.T, conn *gws.Conn) int {
	t.Helper()
	total := 0
	for {
		outerType, payload := readOne(t, conn, 5*time.Second)
		if outerType != 0 { // msgSync
			continue
		}
		total += len(payload)
		sub, _, err := ygsync.ReadSyncMessage(payload)
		require.NoError(t, err)
		if sub == ygsync.MsgSyncStep2 {
			return total
		}
	}
}

// stateAnswer sends joiner's SyncStep1, then each frame of extra, then a
// Ping, and reads up to the Pong, returning every frame the server sent on the
// way. The Ping bounds the read, so a frame that never comes shows as a short
// slice rather than a hang.
func stateAnswer(t *testing.T, conn *gws.Conn, hocuspocus bool, room string, joiner *crdt.Doc, extra ...[]byte) []answerFrame {
	t.Helper()
	send := func(frame []byte) {
		require.NoError(t, conn.WriteMessage(gws.BinaryMessage, frame))
	}
	send(joinFrame(hocuspocus, room, tagSync, ygsync.EncodeSyncStep1(joiner)))
	for _, f := range extra {
		send(f)
	}
	send(joinFrame(hocuspocus, room, tagPing, nil))

	var frames []answerFrame
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
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
			return frames
		}
		f := answerFrame{tag: tag, sub: -1, bytes: len(data)}
		if tag == tagSync {
			f.body = dec.RemainingBytes()
			f.sub, _, err = ygsync.ReadSyncMessage(f.body)
			require.NoError(t, err)
		}
		frames = append(frames, f)
	}
}

// longRun is written in one transaction, so the store holds it as one struct.
// It is longer than a frame, which is why an answer that keeps every frame
// within the cap has to cut a struct rather than pack whole ones.
var longRun = strings.Repeat("L", 80<<10)

// largeState is the fixture every test here uses, built once: building it
// costs more than the tests do, and they all want the same document.
var largeState = sync.OnceValue(buildLargeState)

// buildLargeState returns the V1 state of a document four clients wrote with
// interleaved edits — insertions into each other's text, deletions, and a map
// of nested maps — holding one text item larger than a state frame. Each
// client hears the others' round before writing the next, so the structs of
// every client name structs of the others. A fixture that does not build is a
// fault in the test, so this panics rather than reporting.
func buildLargeState() []byte {
	const clients = 4
	docs := make([]*crdt.Doc, clients)
	for i := range docs {
		docs[i] = crdt.New(crdt.WithClientID(crdt.ClientID(i + 1)))
	}
	converge := func() {
		for i, src := range docs {
			for j, dst := range docs {
				if i == j {
					continue
				}
				if err := crdt.ApplyUpdateV1(dst, crdt.EncodeStateAsUpdateV1(src, dst.StateVector()), nil); err != nil {
					panic("websocket_test: large fixture does not converge: " + err.Error())
				}
			}
		}
	}
	for round := range 30 {
		for i, d := range docs {
			txt := d.GetText("t")
			at := 0
			if n := txt.Len(); n > 0 {
				at = (round*13 + i*7) % n
			}
			d.Transact(func(txn *crdt.Transaction) {
				txt.Insert(txn, at, strings.Repeat(string(rune('a'+i)), 1024), nil)
			})
			if round%5 == 4 {
				d.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, at/2, 256) })
			}
			d.Transact(func(txn *crdt.Transaction) {
				inner := crdt.NewMapPrelim()
				txn.GetMap("m").Set(txn, string(rune('a'+i))+string(rune('0'+round%10)), inner)
				inner.Set(txn, "note", strings.Repeat("n", 40))
			})
		}
		converge()
	}
	// One client writes a run longer than a frame in one transaction.
	longText := docs[0].GetText("t")
	docs[0].Transact(func(txn *crdt.Transaction) { longText.Insert(txn, 0, longRun, nil) })
	converge()
	if len(docs[0].StateVector()) != clients {
		panic("websocket_test: large fixture was not written by every client")
	}
	return crdt.EncodeStateAsUpdateV1(docs[0], nil)
}

// largeStateFixture is largeState with the properties the tests rest on
// asserted: one item larger than a frame, and a state that needs several.
func largeStateFixture(t testing.TB) []byte {
	t.Helper()
	state := largeState()
	require.Greater(t, len(longRun), ygws.StateFrameBytes, "the fixture holds one item larger than a frame")
	require.Greater(t, len(state), 3*ygws.StateFrameBytes, "the fixture needs several frames")
	return state
}

// fixtureServer serves a room seeded with state.
func fixtureServer(t *testing.T, hocuspocus bool, state []byte) (*ygws.Server, *httptest.Server) {
	t.Helper()
	srv := ygws.NewServer()
	srv.HocuspocusFraming = hocuspocus
	srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
		return crdt.ApplyUpdateV1(doc, state, nil)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts
}

// The answer to a SyncStep1 is frames within the cap, each integrating with
// nothing left waiting, and exactly one SyncStep2, last; applying them in
// order rebuilds the room's document.
func TestInteg_StateReply_EveryFrameIntegratesOnItsOwn(t *testing.T) {
	state := largeStateFixture(t)
	for _, hocuspocus := range []bool{false, true} {
		t.Run(framingName(hocuspocus), func(t *testing.T) {
			srv, ts := fixtureServer(t, hocuspocus, state)
			joiner := crdt.New(crdt.WithClientID(99))
			frames := stateAnswer(t, dial(t, ts, "room"), hocuspocus, "room", joiner)

			step2At, lastSyncAt := -1, -1
			for i, f := range frames {
				assert.LessOrEqual(t, f.bytes, ygws.StateFrameBytes, "bytes of frame %d of %d", i+1, len(frames))
				if f.tag != tagSync {
					continue
				}
				lastSyncAt = i
				if f.sub == ygsync.MsgSyncStep2 {
					assert.Equal(t, -1, step2At, "frame %d is a second SyncStep2", i+1)
					step2At = i
				}
				// Applying one frame leaves the receiver holding everything it
				// carried and waiting for nothing: the property a split by byte
				// length would not have.
				_, err := ygsync.ApplySyncMessage(joiner, f.body, nil)
				require.NoError(t, err, "frame %d applies", i+1)
				stats := joiner.PendingStats()
				assert.Zero(t, stats.Items, "structs parked after frame %d of %d", i+1, len(frames))
				assert.Zero(t, stats.DeleteRanges, "delete ranges parked after frame %d of %d", i+1, len(frames))
			}
			require.NotEqual(t, -1, step2At, "the answer carries a SyncStep2")
			assert.Equal(t, step2At, lastSyncAt, "the SyncStep2 is the last sync frame")
			assert.Greater(t, step2At, 1, "the state crossed as Update frames before it")

			room := srv.GetDoc("room")
			assert.Equal(t, room.GetText("t").ToString(), joiner.GetText("t").ToString())
			assert.Equal(t, room.StateVector(), joiner.StateVector())
			wantMap, err := room.GetMap("m").ToJSON()
			require.NoError(t, err)
			gotMap, err := joiner.GetMap("m").ToJSON()
			require.NoError(t, err)
			assert.JSONEq(t, string(wantMap), string(gotMap))
		})
	}
}

// The item longer than a frame is cut across frames: applying the answer up to
// a frame in the middle leaves the receiver holding part of that run, and the
// whole answer leaves it holding all of it.
func TestInteg_StateReply_CutsAnItemLongerThanAFrame(t *testing.T) {
	state := largeStateFixture(t)
	srv, ts := fixtureServer(t, false, state)
	joiner := crdt.New(crdt.WithClientID(99))
	frames := stateAnswer(t, dial(t, ts, "room"), false, "room", joiner)

	partial := false
	for i, f := range frames {
		if f.tag != tagSync {
			continue
		}
		_, err := ygsync.ApplySyncMessage(joiner, f.body, nil)
		require.NoError(t, err, "frame %d applies", i+1)
		held := strings.Count(joiner.GetText("t").ToString(), "L")
		if held > 0 && held < len(longRun) {
			partial = true
		}
	}
	assert.True(t, partial, "the run longer than a frame arrived in pieces")
	assert.Equal(t, srv.GetDoc("room").GetText("t").ToString(), joiner.GetText("t").ToString())
}

// A client that sends an edit right behind its SyncStep1 is answered for it
// only after the state's last frame, so no SyncStatus tells it the sync is
// over before the SyncStep2 does.
func TestInteg_StateReply_NoSyncStatusBeforeTheSyncStep2(t *testing.T) {
	state := largeStateFixture(t)
	_, ts := fixtureServer(t, true, state)
	joiner := crdt.New(crdt.WithClientID(99))
	edit := textUpdate(crdt.New(crdt.WithClientID(777)), "EDIT")
	frames := stateAnswer(t, dial(t, ts, "room"), true, "room", joiner,
		joinFrame(true, "room", tagSync, edit))

	step2At, statusAt := -1, -1
	for i, f := range frames {
		switch {
		case f.tag == tagSyncStatus && statusAt < 0:
			statusAt = i
		case f.tag == tagSync && f.sub == ygsync.MsgSyncStep2:
			step2At = i
		}
	}
	require.NotEqual(t, -1, step2At, "the answer carries a SyncStep2")
	require.NotEqual(t, -1, statusAt, "the edit is answered with a SyncStatus")
	assert.Greater(t, statusAt, step2At, "the SyncStatus follows the SyncStep2")
}

// The per-peer write queue holds every frame of a 1.22 MB state without the
// direct send finding it full, which would close the peer.
func TestUnit_StateReply_WriteQueueHoldsALargeState(t *testing.T) {
	// A measured 1.22 MB document, and the frames the handshake queues ahead
	// of the answer (SyncStep1 and awareness).
	const stateBytes = 1_220_000
	const handshake = 2
	frames := (stateBytes+ygws.StateFrameBytes-1)/ygws.StateFrameBytes + 1 // + the SyncStep2
	assert.LessOrEqual(t, frames+handshake, ygws.DefaultPeerWriteQueueSize,
		"frames of a %d-byte state at %d bytes each", stateBytes, ygws.StateFrameBytes)
}

// A peer that asks for a state of over a megabyte and reads nothing until it
// is all queued is not closed: the frames fit the queue, and the whole state
// reaches it once it reads.
func TestInteg_StateReply_LargeStateFitsTheWriteQueue(t *testing.T) {
	const room = "large-state"
	srv := ygws.NewServer()
	ln := newPipeListener()
	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	dialer := pipeDialer(ln)

	// A resident peer keeps the room loaded; a pipe has no kernel buffer, so
	// the server's writes to a peer that stops reading block at once.
	seeder := dialPipe(t, dialer, room)
	sendUpdate(t, seeder, textUpdate(crdt.New(crdt.WithClientID(1)), strings.Repeat("s", 1_300_000)))
	require.Eventually(t, func() bool {
		return len(crdt.EncodeStateAsUpdateV1(srv.GetDoc(room), nil)) > 1_220_000
	}, 10*time.Second, 10*time.Millisecond, "the room should hold a state of over a megabyte")

	conn, _, err := dialer.Dial("ws://pipe/"+room, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, askFullState(conn))

	joiner := crdt.New(crdt.WithClientID(99))
	for {
		outerType, payload := readOne(t, conn, 10*time.Second)
		if outerType != 0 { // msgSync
			continue
		}
		_, err := ygsync.ApplySyncMessage(joiner, payload, nil)
		require.NoError(t, err)
		if sub, _, err := ygsync.ReadSyncMessage(payload); err == nil && sub == ygsync.MsgSyncStep2 {
			break
		}
	}
	assert.Equal(t, srv.GetDoc(room).GetText("t").ToString(), joiner.GetText("t").ToString())
	_, peers := ygws.PeerQueuedBytes(srv, room)
	assert.Equal(t, 2, peers, "no peer is closed")
}
