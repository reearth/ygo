package websocket_test

import (
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

// wsURL converts an httptest server URL to a ws:// URL.
func wsURL(srv *httptest.Server, room string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/" + room
}

// dial opens a WebSocket connection to the test server.
func dial(t *testing.T, srv *httptest.Server, room string) *gws.Conn {
	t.Helper()
	conn, _, err := gws.DefaultDialer.Dial(wsURL(srv, room), nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readOne reads a single WebSocket message with a deadline, then clears it.
// Returns the outer type and decoded payload.
// For sync (type 0), payload is the raw sync bytes (no length prefix).
// For awareness (type 1), payload is the VarBytes-unwrapped awareness bytes.
func readOne(t *testing.T, conn *gws.Conn, deadline time.Duration) (outerType uint64, payload []byte) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(deadline))
	_, data, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{}) // reset immediately so the conn stays usable
	require.NoError(t, err)

	dec := encoding.NewDecoder(data)
	outerType, err = dec.ReadVarUint()
	require.NoError(t, err)

	if outerType == 1 { // msgAwareness — VarBytes-wrapped
		payload, err = dec.ReadVarBytes()
		require.NoError(t, err)
	} else {
		payload = dec.RemainingBytes()
	}
	return
}

// drainHandshake reads the three messages the server always sends on connect
// (step-1, step-2, awareness) and applies any sync messages to doc.
// Gorilla's bufio.Reader is permanently broken if a deadline expires, so we
// read a known count instead of draining by timeout.
func drainHandshake(t *testing.T, conn *gws.Conn, doc *crdt.Doc) {
	t.Helper()
	for i := 0; i < 3; i++ {
		outerType, payload := readOne(t, conn, 2*time.Second)
		if outerType == 0 { // msgSync
			_, _ = ygsync.ApplySyncMessage(doc, payload, nil)
		}
	}
}

// sendSync wraps payload in an outer msgSync message and sends it.
// Sync payload is NOT VarBytes-wrapped (raw append after type byte).
func sendSync(t *testing.T, conn *gws.Conn, syncMsg []byte) {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // msgSync
	enc.WriteRaw(syncMsg)
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))
}

// sendAwareness wraps payload in an outer msgAwareness message.
// Awareness payload IS VarBytes-wrapped.
func sendAwareness(t *testing.T, conn *gws.Conn, awMsg []byte) {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // msgAwareness
	enc.WriteVarBytes(awMsg)
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))
}

// ── Unit tests ────────────────────────────────────────────────────────────────

func TestUnit_NewServer_GetDoc_ReturnsNilForUnknownRoom(t *testing.T) {
	srv := ygws.NewServer()
	assert.Nil(t, srv.GetDoc("no-such-room"))
}

func TestUnit_ServerHandshake_SendsStep1AndStep2ThenAwareness(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "room1")

	// Message 1: msgSync + step-1
	outerType, payload := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType, "first msg should be msgSync")
	dec := encoding.NewDecoder(payload)
	syncType, err := dec.ReadVarUint()
	require.NoError(t, err)
	assert.Equal(t, uint64(ygsync.MsgSyncStep1), syncType)

	// Message 2: msgSync + step-2
	outerType, payload = readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType, "second msg should be msgSync")
	dec = encoding.NewDecoder(payload)
	syncType, err = dec.ReadVarUint()
	require.NoError(t, err)
	assert.Equal(t, uint64(ygsync.MsgSyncStep2), syncType)

	// Message 3: msgAwareness
	outerType, _ = readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(1), outerType, "third msg should be msgAwareness")
}

func TestUnit_GetDoc_PopulatedAfterFirstConnection(t *testing.T) {
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "myroom")
	drainHandshake(t, conn, crdt.New())

	assert.NotNil(t, srv.GetDoc("myroom"))
}

// ── Integration tests ─────────────────────────────────────────────────────────

func TestInteg_TwoPeer_DocumentConvergence(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// Peer A connects, drains handshake, then sends "hello" to the server.
	docA := crdt.New(crdt.WithClientID(1))
	connA := dial(t, ts, "room")
	drainHandshake(t, connA, docA)

	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hello", nil) })
	update := crdt.EncodeStateAsUpdateV1(docA, nil)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, connA, enc.Bytes())

	time.Sleep(50 * time.Millisecond)

	// Peer B connects and drains the handshake; step-2 should contain "hello".
	docB := crdt.New(crdt.WithClientID(2))
	connB := dial(t, ts, "room")
	drainHandshake(t, connB, docB)

	assert.Equal(t, "hello", docB.GetText("t").ToString())
}

func TestInteg_StepOneReply_ServerRespondsWithStep2(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// First peer seeds the room with "world".
	seedDoc := crdt.New(crdt.WithClientID(10))
	connSeed := dial(t, ts, "seeded")
	drainHandshake(t, connSeed, seedDoc)

	seedTxt := seedDoc.GetText("t")
	seedDoc.Transact(func(txn *crdt.Transaction) { seedTxt.Insert(txn, 0, "world", nil) })
	update := crdt.EncodeStateAsUpdateV1(seedDoc, nil)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, connSeed, enc.Bytes())
	time.Sleep(50 * time.Millisecond)

	// Second peer connects; step-2 in handshake should contain "world".
	docNew := crdt.New(crdt.WithClientID(20))
	connNew := dial(t, ts, "seeded")
	drainHandshake(t, connNew, docNew)

	assert.Equal(t, "world", docNew.GetText("t").ToString())
}

func TestInteg_IncrementalUpdate_BroadcastToPeer(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// Both peers connect.
	docA := crdt.New(crdt.WithClientID(1))
	docB := crdt.New(crdt.WithClientID(2))
	connA := dial(t, ts, "room")
	connB := dial(t, ts, "room")
	drainHandshake(t, connA, docA)
	drainHandshake(t, connB, docB)

	// A sends an update.
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "incremental", nil) })
	update := crdt.EncodeStateAsUpdateV1(docA, nil)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, connA, enc.Bytes())

	// B should receive the broadcast.
	outerType, payload := readOne(t, connB, 2*time.Second)
	assert.Equal(t, uint64(0), outerType)

	// Apply to B's doc.
	_, _ = ygsync.ApplySyncMessage(docB, payload, nil)
	assert.Equal(t, "incremental", docB.GetText("t").ToString())
}

func TestInteg_AwarenessBroadcast_PeerReceivesState(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// Both peers connect and drain their handshakes.
	connA := dial(t, ts, "awroom")
	connB := dial(t, ts, "awroom")
	drainHandshake(t, connA, crdt.New())
	drainHandshake(t, connB, crdt.New())

	// A sends an awareness update with clientID=42.
	aw := awareness.New(42)
	aw.SetLocalState(map[string]any{"cursor": float64(5)})
	sendAwareness(t, connA, aw.EncodeUpdate(nil))

	// B should receive the awareness broadcast.
	outerType, awPayload := readOne(t, connB, 2*time.Second)
	assert.Equal(t, uint64(1), outerType)

	// Parse and verify.
	awDec := awareness.New(99)
	require.NoError(t, awDec.ApplyUpdate(awPayload, nil))
	states := awDec.GetStates()
	require.Contains(t, states, uint64(42))
	assert.Equal(t, float64(5), states[42].State["cursor"])
}

func TestInteg_QueryAwareness_ReturnsCurrentState(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "qroom")
	drainHandshake(t, conn, crdt.New())

	// Send msgQueryAwareness (type 3).
	enc := encoding.NewEncoder()
	enc.WriteVarUint(3)
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))

	outerType, _ := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(1), outerType)
}

func TestInteg_MultiRoom_Isolated(t *testing.T) {
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Peer in room-A sends "alpha".
	docA := crdt.New(crdt.WithClientID(1))
	connA := dial(t, ts, "room-a")
	drainHandshake(t, connA, docA)

	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "alpha", nil) })
	update := crdt.EncodeStateAsUpdateV1(docA, nil)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, connA, enc.Bytes())
	time.Sleep(50 * time.Millisecond)

	// Peer connecting to room-B must NOT see "alpha".
	docB := crdt.New(crdt.WithClientID(2))
	connB := dial(t, ts, "room-b")
	drainHandshake(t, connB, docB)

	assert.Equal(t, "", docB.GetText("t").ToString())
	assert.NotNil(t, srv.GetDoc("room-a"))
	assert.NotNil(t, srv.GetDoc("room-b"))
}
