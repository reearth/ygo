package websocket_test

import (
	"errors"
	"net/http/httptest"
	"strconv"
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

// Tests for the SyncStatus (tag 8) acknowledgement a Hocuspocus-framed peer
// receives for every SyncStep2 or Update it sends: 1 once the room applied it,
// 0 when the room did not.

const (
	tagSync       = uint64(0)
	tagSyncReply  = uint64(4)
	tagSyncStatus = uint64(8)
	tagPing       = uint64(9)
	tagPong       = uint64(10)
)

// hocuspocusServer starts a server using Hocuspocus framing. configure, when
// non-nil, runs before the server starts.
func hocuspocusServer(t *testing.T, configure func(*ygws.Server)) (*ygws.Server, *httptest.Server) {
	t.Helper()
	srv := ygws.NewServer()
	srv.HocuspocusFraming = true
	if configure != nil {
		configure(srv)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts
}

// hpSend writes one Hocuspocus frame: VarString(room), the outer tag, then body.
func hpSend(t *testing.T, conn *gws.Conn, room string, tag uint64, body []byte) {
	t.Helper()
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(tag)
		enc.WriteRaw(body)
	})))
}

// hpRead reads one Hocuspocus frame and returns its outer tag and body, or
// the read error. The deadline is cleared before returning.
func hpRead(conn *gws.Conn, room string, deadline time.Duration) (uint64, []byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(deadline))
	_, data, err := conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return 0, nil, err
	}
	dec := encoding.NewDecoder(data)
	name, err := dec.ReadVarString()
	if err != nil {
		return 0, nil, err
	}
	if name != room {
		return 0, nil, errors.New("frame names room " + strconv.Quote(name))
	}
	tag, err := dec.ReadVarUint()
	if err != nil {
		return 0, nil, err
	}
	return tag, dec.RemainingBytes(), nil
}

// hpDrainHandshake reads the three frames the server sends on connect
// (SyncStep1, SyncStep2, awareness) and applies the sync ones to doc.
func hpDrainHandshake(t *testing.T, conn *gws.Conn, room string, doc *crdt.Doc) {
	t.Helper()
	for range 3 {
		tag, body, err := hpRead(conn, room, 2*time.Second)
		require.NoError(t, err)
		if tag == tagSync {
			_, _ = ygsync.ApplySyncMessage(doc, body, nil)
		}
	}
}

// wholeStep2 returns the SyncStep2 message carrying doc's whole state, as a
// client answers a server's SyncStep1 naming no state.
func wholeStep2(t *testing.T, doc *crdt.Doc) []byte {
	t.Helper()
	msg, err := ygsync.EncodeSyncStep2(doc, ygsync.EncodeSyncStep1(crdt.New()))
	require.NoError(t, err)
	return msg
}

// hpDial connects to room, marked read-only when readOnly is set (the
// server's Authorize must be readOnlyByHeader), and drains the handshake into
// doc.
func hpDial(t *testing.T, ts *httptest.Server, room string, readOnly bool, doc *crdt.Doc) *gws.Conn {
	t.Helper()
	conn := dialReadOnly(t, ts, room, readOnly)
	hpDrainHandshake(t, conn, room, doc)
	return conn
}

// syncStatuses sends a Ping and reads frames until its Pong, returning the
// flag of every SyncStatus frame on the way, in order. Frames of other kinds
// (a broadcast, awareness) are skipped. The Pong bounds the read without a
// timeout, so a missing SyncStatus shows as a short slice rather than a hang.
func syncStatuses(t *testing.T, conn *gws.Conn, room string) []uint64 {
	t.Helper()
	hpSend(t, conn, room, tagPing, nil)
	var flags []uint64
	for {
		tag, body, err := hpRead(conn, room, 2*time.Second)
		require.NoError(t, err)
		switch tag {
		case tagPong:
			return flags
		case tagSyncStatus:
			flag, err := encoding.NewDecoder(body).ReadVarUint()
			require.NoError(t, err)
			flags = append(flags, flag)
		}
	}
}

// textUpdate returns the update client makes by inserting text at the start
// of YText "t" in doc.
func textUpdate(doc *crdt.Doc, text string) []byte {
	before := doc.StateVector()
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, text, nil) })
	return crdt.EncodeStateAsUpdateV1(doc, before)
}

// parkingUpdate returns an update in which client 2 sets n keys on a map that
// client 1 created and the room never received, so every entry parks waiting
// for its parent.
func parkingUpdate(t *testing.T, n int) []byte {
	t.Helper()
	author := crdt.New(crdt.WithClientID(1))
	root := author.GetMap("m")
	author.Transact(func(txn *crdt.Transaction) { root.Set(txn, "nested", crdt.NewMapPrelim()) })
	editor := crdt.New(crdt.WithClientID(2))
	require.NoError(t, crdt.ApplyUpdateV1(editor, crdt.EncodeStateAsUpdateV1(author, nil), nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested, ok := v.(*crdt.YMap)
	require.True(t, ok, "nested is %T", v)
	editor.Transact(func(txn *crdt.Transaction) {
		for i := range n {
			nested.Set(txn, "k"+strconv.Itoa(i), i)
		}
	})
	return crdt.EncodeStateAsUpdateV1(editor, author.StateVector())
}

// A read-write peer's Update is answered SyncStatus(1), and by the time the
// answer arrives the room holds the update.
func TestInteg_SyncStatus_AppliedUpdateAnsweredOnceTheRoomHoldsIt(t *testing.T) {
	srv, ts := hocuspocusServer(t, nil)
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "hello")))

	tag, body, err := hpRead(conn, "room", 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, tagSyncStatus, tag, "the next frame answers the update")
	flag, err := encoding.NewDecoder(body).ReadVarUint()
	require.NoError(t, err)
	assert.Equal(t, uint64(1), flag)
	assert.Equal(t, "hello", srv.GetDoc("room").GetText("t").ToString(),
		"the room holds the update when its SyncStatus arrives")
}

// Every SyncStep2 or Update, under Sync (tag 0) or SyncReply (tag 4), gets
// exactly one SyncStatus, in the order the frames arrived, so a client can
// pair each answer with the frame it sent.
func TestInteg_SyncStatus_OneAnswerPerFrameInArrivalOrder(t *testing.T) {
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) { srv.MaxPendingItems = 2 })
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "a")))
	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(parkingUpdate(t, 5)))
	textUpdate(doc, "b")
	hpSend(t, conn, "room", tagSync, wholeStep2(t, doc))
	hpSend(t, conn, "room", tagSyncReply, ygsync.EncodeUpdate(textUpdate(doc, "c")))

	assert.Equal(t, []uint64{1, 0, 1, 1}, syncStatuses(t, conn, "room"))
}

// An update the room refuses (here, one that overflows its pending cap) is
// answered SyncStatus(0) and the socket stays: the next good update on it is
// answered SyncStatus(1). @hocuspocus/server answers such an update 1, since
// Yjs has no pending cap and parks it; ygo answers what the room did.
func TestInteg_SyncStatus_RefusedUpdateAnsweredZeroAndSocketStays(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) { srv.MaxPendingItems = 2 })
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", false, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(parkingUpdate(t, 5)))
	assert.Equal(t, []uint64{0}, syncStatuses(t, conn, "room"))

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "after")))
	assert.Equal(t, []uint64{1}, syncStatuses(t, conn, "room"))
	assert.Equal(t, "after", srv.GetDoc("room").GetText("t").ToString())
}

// A read-only peer's Update is answered SyncStatus(0) and the room is
// unchanged, as @hocuspocus/server answers it.
func TestInteg_SyncStatus_ReadOnlyUpdateAnsweredZero(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) { srv.Authorize = readOnlyByHeader })
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDial(t, ts, "room", true, doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "hello")))
	hpSend(t, conn, "room", tagSyncReply, ygsync.EncodeUpdate(textUpdate(doc, "again")))

	assert.Equal(t, []uint64{0, 0}, syncStatuses(t, conn, "room"))
	assert.Empty(t, srv.GetDoc("room").GetText("t").ToString(),
		"a read-only peer's update must not reach the room")
}

// A read-only peer's SyncStep2 is answered SyncStatus(1) when the room already
// holds everything in it and SyncStatus(0) when it carries something new,
// which the room does not take: @hocuspocus/server's snapshotContainsUpdate
// check.
func TestInteg_SyncStatus_ReadOnlySyncStep2AnsweredByWhetherTheRoomHoldsIt(t *testing.T) {
	srv, ts := hocuspocusServer(t, func(srv *ygws.Server) { srv.Authorize = readOnlyByHeader })
	editorDoc := crdt.New(crdt.WithClientID(1))
	editor := hpDial(t, ts, "room", false, editorDoc)
	hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(textUpdate(editorDoc, "hello")))
	editorText := editorDoc.GetText("t")
	editorDoc.Transact(func(txn *crdt.Transaction) { editorText.Delete(txn, 0, 1) })
	hpSend(t, editor, "room", tagSync, wholeStep2(t, editorDoc))
	require.Equal(t, []uint64{1, 1}, syncStatuses(t, editor, "room"))

	readerDoc := crdt.New(crdt.WithClientID(2))
	reader := hpDial(t, ts, "room", true, readerDoc)
	require.Equal(t, "ello", readerDoc.GetText("t").ToString())
	step2 := func() []byte { return wholeStep2(t, readerDoc) }

	hpSend(t, reader, "room", tagSync, step2())
	assert.Equal(t, []uint64{1}, syncStatuses(t, reader, "room"),
		"a SyncStep2 the room already holds, deletions included, is answered 1")

	textUpdate(readerDoc, "x")
	hpSend(t, reader, "room", tagSync, step2())
	assert.Equal(t, []uint64{0}, syncStatuses(t, reader, "room"),
		"a SyncStep2 with a new insertion is answered 0")

	readerDoc2 := crdt.New(crdt.WithClientID(3))
	reader2 := hpDial(t, ts, "room", true, readerDoc2)
	reader2Text := readerDoc2.GetText("t")
	readerDoc2.Transact(func(txn *crdt.Transaction) { reader2Text.Delete(txn, 0, 1) })
	hpSend(t, reader2, "room", tagSync, wholeStep2(t, readerDoc2))
	assert.Equal(t, []uint64{0}, syncStatuses(t, reader2, "room"),
		"a SyncStep2 with a new deletion is answered 0")

	assert.Equal(t, "ello", srv.GetDoc("room").GetText("t").ToString(),
		"a read-only peer's SyncStep2 must not reach the room")
}

// A sync frame that does not decode gets no SyncStatus, and the server closes
// the socket with 1002 (protocol error). @hocuspocus/server answers a truncated
// SyncStep2 or Update with 1 instead; ygo does not acknowledge an edit the room
// never saw. The client's next connection starts its pairing afresh.
func TestInteg_SyncStatus_UndecodableSyncFrameClosesWithoutAnswer(t *testing.T) {
	_, ts := hocuspocusServer(t, nil)
	conn := hpDial(t, ts, "room", false, crdt.New())

	// SyncStep2 (sub-type 1) claiming a 5-byte update with none following.
	hpSend(t, conn, "room", tagSync, []byte{byte(ygsync.MsgSyncStep2), 0x05})

	for {
		tag, _, err := hpRead(conn, "room", 2*time.Second)
		if err != nil {
			var closed *gws.CloseError
			require.ErrorAs(t, err, &closed, "the server closes the socket")
			assert.Equal(t, gws.CloseProtocolError, closed.Code)
			return
		}
		require.NotEqual(t, tagSyncStatus, tag, "an undecodable frame gets no SyncStatus")
	}
}

// A peer on plain y-websocket framing receives no SyncStatus: y-websocket
// clients do not know tag 8.
func TestInteg_SyncStatus_NotSentOnYWebsocketFraming(t *testing.T) {
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	conn := dial(t, ts, "room")
	doc := crdt.New(crdt.WithClientID(7))
	drainHandshake(t, conn, doc)

	sendUpdate(t, conn, textUpdate(doc, "hello"))
	sendSync(t, conn, wholeStep2(t, doc))
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, []byte{byte(tagPing)}))

	for {
		outerType, _ := readOne(t, conn, 2*time.Second)
		if outerType == tagPong {
			break
		}
		require.NotEqual(t, tagSyncStatus, outerType, "no SyncStatus on y-websocket framing")
	}
	assert.Equal(t, "hello", srv.GetDoc("room").GetText("t").ToString())
}
