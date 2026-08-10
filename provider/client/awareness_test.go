package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// TestClient_Awareness_LocalPresenceReachesPeer is #165 Task 8's baseline
// claim: a local SetLocalState call on one Client's Awareness must reach a
// second Client connected to the same room, over the real y-websocket wire —
// not merely update the local Awareness map in place. Before this task
// lands, loop.go's outbound TakeAwareness branch has no producer (nothing
// calls Awareness.OnUpdate to push onto the lane) and its inbound
// wireMsgAwareness case is a documented no-op, so this must fail before the
// implementation exists.
func TestClient_Awareness_LocalPresenceReachesPeer(t *testing.T) {
	_, ts := startServer(t)
	const room = "awareness-basic"

	a, _ := dialSynced(t, ts, room, Options{})
	b, _ := dialSynced(t, ts, room, Options{})

	a.Awareness().SetLocalState(map[string]any{"name": "a"})

	require.Eventually(t, func() bool {
		cs, ok := b.Awareness().GetStates()[a.Awareness().ClientID()]
		return ok && cs.State["name"] == "a"
	}, 5*time.Second, 10*time.Millisecond, "B never observed A's local awareness state")
}

// TestClient_Awareness_ReconnectRebroadcastsWithAdvancedClock is the
// reconnect half of #165 Task 8. srv.CloseRoom(room, true) — the same
// forced-disconnect technique reconnect_test.go uses — destroys the room
// AND its awareness.Awareness entirely (see provider/websocket's room
// cleanup, which calls awareness.Destroy()), so whatever A and B see after
// reconnecting can only have arrived via a fresh broadcast on the new
// connection, not residual server state.
//
// The test proves two things, deliberately kept as separate assertions
// rather than folded into one: (1) A's OWN local awareness clock — read
// directly off A's Awareness via Meta, nothing server- or B-side — is
// strictly greater after reconnecting than it was before the disconnect,
// which can only be true if something bumped it (Heartbeat) on the new
// connection; and (2) B, a completely independent observer, again sees A's
// state after reconnecting into what is, from the room's perspective, a
// brand-new room that never heard of A before this moment. (1) alone could
// theoretically pass by coincidence if some unrelated clock-bumping call
// existed; (2) alone could pass if the client merely resent STALE state at
// its old clock and the room happened to accept it (it would: a fresh room
// has no prior clock to gate against). Together they pin down that the
// re-announcement is both NEW (bumped clock) and DELIVERED (observed by a
// peer), which is exactly the "must not look expired to peers" property
// this task exists to provide.
func TestClient_Awareness_ReconnectRebroadcastsWithAdvancedClock(t *testing.T) {
	srv, ts := startServer(t)
	const room = "awareness-reconnect"

	a, _ := dialSynced(t, ts, room, Options{MaxBackoff: time.Second})
	b, _ := dialSynced(t, ts, room, Options{MaxBackoff: time.Second})

	a.Awareness().SetLocalState(map[string]any{"name": "a"})
	require.Eventually(t, func() bool {
		cs, ok := b.Awareness().GetStates()[a.Awareness().ClientID()]
		return ok && cs.State["name"] == "a"
	}, 5*time.Second, 10*time.Millisecond, "B never observed A's local awareness state before disconnect")

	beforeMeta, ok := a.Awareness().Meta(a.Awareness().ClientID())
	require.True(t, ok, "A has no awareness meta for its own clientID before disconnect")
	beforeClock := beforeMeta.Clock

	// Arm every status subscription needed for the whole disconnect+reconnect
	// cycle BEFORE the triggering CloseRoom call, per statusWaiter's
	// documented discipline (harness_test.go): OnStatus does not replay, and
	// arming the StateSynced waiters only after observing StateDisconnected
	// would leave a real (if narrow) window for a fast reconnect to complete
	// unobserved between the two calls.
	waitADisc := statusWaiter(t, a, StateDisconnected)
	waitBDisc := statusWaiter(t, b, StateDisconnected)
	waitASynced := statusWaiter(t, a, StateSynced)
	waitBSynced := statusWaiter(t, b, StateSynced)

	require.NoError(t, srv.CloseRoom(room, true))
	waitADisc()
	waitBDisc()
	waitASynced()
	waitBSynced()

	require.Eventually(t, func() bool {
		m, ok := a.Awareness().Meta(a.Awareness().ClientID())
		return ok && m.Clock > beforeClock
	}, 5*time.Second, 10*time.Millisecond,
		"A's own awareness clock never advanced after reconnecting; a rejoining "+
			"client must bump its clock so it does not look expired to peers")

	require.Eventually(t, func() bool {
		cs, ok := b.Awareness().GetStates()[a.Awareness().ClientID()]
		return ok && cs.State["name"] == "a"
	}, 5*time.Second, 10*time.Millisecond,
		"B never re-observed A's presence after the reconnect into a fresh room")
}

// TestClient_Awareness_QueryAnsweredWithFullLocalState protects the
// queryAwareness responder (#165 Task 8, wireMsgQueryAwareness). It uses a
// bare hand-rolled WebSocket handler — not ygo's own provider/websocket.Server
// — specifically so it can send a queryAwareness frame on demand and inspect
// exactly what comes back, which the real server's own handshake sequence
// (which already sends the room's awareness snapshot unconditionally on
// join) cannot isolate. The handshake is deliberately never completed here
// (no SyncStep2 is ever sent back): the query-response path must not depend
// on synced state, matching provider/websocket/peer.go's own
// `case msgQueryAwareness`, which answers unconditionally.
func TestClient_Awareness_QueryAnsweredWithFullLocalState(t *testing.T) {
	upgrader := gws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	respCh := make(chan []byte, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Drain the client's initial SyncStep1; its content plays no part in
		// this test, which never completes the handshake.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(gws.BinaryMessage, encodeEnvelope(wireMsgQueryAwareness, nil)); err != nil {
			return
		}
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msgType, payload, err := decodeEnvelope(data)
			if err != nil {
				continue
			}
			if msgType != wireMsgAwareness {
				continue
			}
			// Awareness frames are VarBytes-wrapped on the wire (see
			// decodeEnvelope's doc and provider/websocket/peer.go's
			// `case msgAwareness`); unwrap before handing the bytes off.
			awBytes, err := encoding.NewDecoder(payload).ReadVarBytes()
			if err != nil {
				return
			}
			respCh <- append([]byte(nil), awBytes...)
			return
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c, err := New(Options{
		URL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/query-awareness",
		Doc: crdt.New(),
	})
	require.NoError(t, err)
	// Set local state BEFORE Connect starts the dial loop: the lane never
	// blocks (see relaylane's doc), so this is queued and waiting the moment
	// the first connection's flushLane runs, with no race against how fast
	// the raw handler above gets around to sending its query.
	c.Awareness().SetLocalState(map[string]any{"name": "a"})
	connect(t, c)

	var awBytes []byte
	select {
	case awBytes = <-respCh:
	case <-time.After(5 * time.Second):
		t.Fatal("client never answered the inbound queryAwareness frame")
	}

	// Decode with a scratch Awareness instance using the SAME library
	// primitives a real peer would use, rather than hand-parsing the wire
	// format here — this is what "answered with A's full local state" means
	// operationally: a peer that runs this exact decode ends up knowing A's
	// state.
	scratch := awareness.New(0)
	require.NoError(t, scratch.ApplyUpdate(awBytes, nil))
	cs, ok := scratch.GetStates()[c.Awareness().ClientID()]
	require.True(t, ok, "query reply did not include A's own clientID")
	require.Equal(t, "a", cs.State["name"])
}

// TestClient_Awareness_HeartbeatSurvivesServerExpiry protects the periodic
// heartbeat (#165 Task 8): a client that sets local state once and then goes
// quiet must not be reaped by the server's AwarenessExpiry sweep
// (provider/websocket/server.go's AwarenessExpiry doc explicitly calls out
// that a client's own re-announce interval must stay comfortably under this
// window). AwarenessExpiry is set far shorter than its documented production
// values specifically so the test itself runs in well under a second; A's
// PingInterval (which now also drives the awareness heartbeat, see
// runLoop's ping ticker case) is set well under HALF of AwarenessExpiry — the
// server's own sweep tick — so every sweep observes a recently-refreshed
// entry.
//
// require.Never (not require.Eventually) is deliberate: the property under
// test is "at no point during this window does B stop seeing A", not "B
// eventually sees A again" — the latter would also be satisfied by a client
// that flickered offline and reappeared, which is exactly the false-negative
// a naive fixed-sleep-then-check test would miss.
func TestClient_Awareness_HeartbeatSurvivesServerExpiry(t *testing.T) {
	const awarenessExpiry = 200 * time.Millisecond
	_, ts := startServer(t, func(s *ygws.Server) { s.AwarenessExpiry = awarenessExpiry })
	const room = "awareness-heartbeat"

	const pingInterval = 50 * time.Millisecond // well under AwarenessExpiry/2's sweep tick
	a, _ := dialSynced(t, ts, room, Options{PingInterval: pingInterval})
	b, _ := dialSynced(t, ts, room, Options{})

	a.Awareness().SetLocalState(map[string]any{"name": "a"})
	require.Eventually(t, func() bool {
		cs, ok := b.Awareness().GetStates()[a.Awareness().ClientID()]
		return ok && cs.State["name"] == "a"
	}, 5*time.Second, 10*time.Millisecond, "B never observed A's local awareness state")

	require.Never(t, func() bool {
		_, ok := b.Awareness().GetStates()[a.Awareness().ClientID()]
		return !ok
	}, 5*awarenessExpiry, 20*time.Millisecond,
		"A's presence disappeared from B despite A's PingInterval heartbeat being "+
			"well under the server's AwarenessExpiry sweep")
}
