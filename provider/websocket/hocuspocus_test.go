package websocket_test

import (
	"net/http/httptest"
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

// Tests for #55 — Hocuspocus message types 4-10 routed through
// peer.handleMessage. Each test exercises one tag end-to-end:
// peer sends → server dispatches → expected side-effect (hook fires,
// peer receives broadcast, connection closes, etc.).

// sendStateless sends a Hocuspocus Stateless (tag 5) message.
func sendStateless(t *testing.T, conn *gws.Conn, payload string) {
	t.Helper()
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(5) // msgStateless
		enc.WriteVarString(payload)
	})))
}

// sendBroadcastStateless sends a Hocuspocus BroadcastStateless (tag 6).
func sendBroadcastStateless(t *testing.T, conn *gws.Conn, payload string) {
	t.Helper()
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(6) // msgBroadcastStateless
		enc.WriteVarString(payload)
	})))
}

// sendPing sends a Hocuspocus Ping (tag 9) frame — a single byte.
func sendPing(t *testing.T, conn *gws.Conn) {
	t.Helper()
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, []byte{0x09}))
}

// #55 — SyncReply (tag 4) must apply the payload locally and broadcast
// any update to other peers, but must NOT echo a sync reply back to the
// sender. (Plain Sync tag 0 sends a step-2 reply when the sender sends a
// step-1; SyncReply exists specifically to break that loop on links that
// would otherwise ping-pong forever.)
func TestInteg_Hocuspocus_SyncReply_AppliesAndBroadcastsNoEcho(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// Two peers in the same room. A will send a SyncReply carrying a
	// real update; B should receive the broadcast.
	connA := dial(t, ts, "syncreplyroom")
	connB := dial(t, ts, "syncreplyroom")
	docA := crdt.New(crdt.WithClientID(1))
	docB := crdt.New(crdt.WithClientID(2))
	drainHandshake(t, connA, docA)
	drainHandshake(t, connB, docB)

	// A produces a local update and wraps it as a sync MsgUpdate payload
	// (VarUint tag + VarBytes(update)), then sends it framed as
	// msgSyncReply (tag 4) instead of msgSync (tag 0). SyncReply carries
	// the same inner sync payload shapes as Sync — typically MsgUpdate or
	// MsgSyncStep2 — but is dispatched without the auto step-1 echo back.
	txt := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })
	update := crdt.EncodeStateAsUpdateV1(docA, nil)

	require.NoError(t, connA.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(4)                        // msgSyncReply
		enc.WriteVarUint(uint64(ygsync.MsgUpdate)) // inner sync subtype
		enc.WriteVarBytes(update)
	})))

	// B must receive the broadcast as a regular Sync (tag 0) frame.
	outerType, _ := readOne(t, connB, 2*time.Second)
	assert.Equal(t, uint64(0), outerType,
		"SyncReply must be broadcast to other peers as a regular Sync (tag 0)")

	// A must NOT receive any echo. Confirm with a short deadline.
	_ = connA.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, _, err := connA.ReadMessage()
	require.Error(t, err,
		"SyncReply must NOT trigger an echo back to the sender")
	_ = connA.SetReadDeadline(time.Time{})
}

// #55 — Stateless (tag 5) must fire OnStateless with IsBroadcast=false
// and must NOT broadcast to other peers.
func TestInteg_Hocuspocus_Stateless_FiresHookNoBroadcast(t *testing.T) {
	srv := ygws.NewServer()

	var (
		mu        sync.Mutex
		seenInfos []ygws.StatelessInfo
	)
	srv.OnStateless = func(info ygws.StatelessInfo) {
		mu.Lock()
		defer mu.Unlock()
		seenInfos = append(seenInfos, info)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	connA := dial(t, ts, "statelessroom")
	connB := dial(t, ts, "statelessroom")
	drainHandshake(t, connA, crdt.New())
	drainHandshake(t, connB, crdt.New())

	sendStateless(t, connA, "hello from A")

	// Hook must fire on the server. Poll briefly because dispatch is async
	// (peer read goroutine -> hook call).
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seenInfos) == 1
	}, 2*time.Second, 10*time.Millisecond,
		"OnStateless must fire exactly once for tag-5 Stateless")

	mu.Lock()
	got := seenInfos[0]
	mu.Unlock()
	assert.Equal(t, "statelessroom", got.Room)
	assert.Equal(t, "hello from A", got.Payload)
	assert.False(t, got.IsBroadcast, "tag 5 must produce IsBroadcast=false")

	// B must NOT receive any broadcast — set a short deadline and confirm
	// no message arrives. (Read deadline expiry on gorilla returns an
	// i/o-timeout error, which is what we expect.)
	_ = connB.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, _, err := connB.ReadMessage()
	require.Error(t, err, "Stateless (tag 5) must NOT broadcast to other peers")
	_ = connB.SetReadDeadline(time.Time{})
}

// #55 — BroadcastStateless (tag 6) must fan out to other peers as a
// plain Stateless (tag 5) frame, AND fire OnStateless with
// IsBroadcast=true on the server side.
func TestInteg_Hocuspocus_BroadcastStateless_FanOutAsStateless(t *testing.T) {
	srv := ygws.NewServer()

	var (
		mu       sync.Mutex
		hookCall ygws.StatelessInfo
		hookHit  bool
	)
	srv.OnStateless = func(info ygws.StatelessInfo) {
		mu.Lock()
		defer mu.Unlock()
		hookCall = info
		hookHit = true
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	connA := dial(t, ts, "broadcastroom")
	connB := dial(t, ts, "broadcastroom")
	drainHandshake(t, connA, crdt.New())
	drainHandshake(t, connB, crdt.New())

	sendBroadcastStateless(t, connA, "shouted from A")

	// B must receive a Stateless (tag 5) frame carrying the payload.
	outerType, payload := readOne(t, connB, 2*time.Second)
	assert.Equal(t, uint64(5), outerType,
		"BroadcastStateless must arrive at other peers as tag 5 (Stateless)")

	// payload here is the raw bytes after the tag, which is a
	// VarString-wrapped string.
	dec := encoding.NewDecoder(payload)
	got, err := dec.ReadVarString()
	require.NoError(t, err)
	assert.Equal(t, "shouted from A", got)

	// Hook fired with IsBroadcast=true.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return hookHit
	}, 2*time.Second, 10*time.Millisecond)
	mu.Lock()
	assert.True(t, hookCall.IsBroadcast,
		"tag 6 must produce IsBroadcast=true on OnStateless")
	assert.Equal(t, "shouted from A", hookCall.Payload)
	mu.Unlock()
}

// #55 — Ping (tag 9) must elicit a single-byte Pong (tag 10) reply.
func TestInteg_Hocuspocus_Ping_RepliesWithPong(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "pingroom")
	drainHandshake(t, conn, crdt.New())

	sendPing(t, conn)

	outerType, payload := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(10), outerType, "Ping must trigger a tag-10 Pong reply")
	assert.Empty(t, payload, "Pong frame carries no payload — just the tag byte")
}

// #55 — Pong (tag 10) from a peer must be silently consumed (no reply,
// no broadcast, connection stays open).
func TestInteg_Hocuspocus_Pong_SilentlyConsumed(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "pongroom")
	drainHandshake(t, conn, crdt.New())

	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, []byte{0x0A})) // tag 10

	// No reply expected; the connection must stay open. Confirm by
	// sending a Ping and getting a Pong back — proves the read loop
	// hasn't aborted on the Pong.
	sendPing(t, conn)
	outerType, _ := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(10), outerType,
		"connection must stay open after Pong; subsequent Ping must still elicit Pong")
}

// #55 — CLOSE (tag 7) from a peer must close the underlying connection.
// The reason VarString is optional.
func TestInteg_Hocuspocus_Close_ClosesConnection(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "closeroom")
	drainHandshake(t, conn, crdt.New())

	// Send tag 7 + a reason VarString.
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(7) // msgClose
		enc.WriteVarString("client-initiated test close")
	})))

	// Next read must fail because the server closed the connection.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	require.Error(t, err,
		"CLOSE (tag 7) must close the underlying WebSocket; subsequent read must fail")
}

// #55 — SyncStatus (tag 8) is normally server→client; if a client sends
// it the server must consume the VarUint flag silently without dropping
// the connection.
func TestInteg_Hocuspocus_SyncStatus_SilentlyConsumed(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "statusroom")
	drainHandshake(t, conn, crdt.New())

	// Send tag 8 with flag=1.
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(8) // msgSyncStatus
		enc.WriteVarUint(1) // applied
	})))

	// Connection must stay open — confirm via Ping/Pong round-trip.
	sendPing(t, conn)
	outerType, _ := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(10), outerType,
		"SyncStatus from a client must not affect liveness")
}
