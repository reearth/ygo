package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
// # Why the proof is a THIRD, LATE-JOINING peer, not a peer connected throughout
//
// An earlier version of this test kept a second peer B connected across the
// whole window and asserted B kept seeing A. That is vacuous: it passes
// identically whether or not either Heartbeat call site exists, because
// nothing in provider/websocket's own RemoveExpired sweep tells an
// ALREADY-CONNECTED peer that a room's Awareness changed — the only
// broadcastAwareness call sites are the inbound-frame path and disconnect
// (see peer.go); an expiry sweep mutates the room's Awareness in place with
// no broadcast of its own. B would keep showing A regardless, off residual
// state alone. The one thing that DOES reflect the room's server-side
// Awareness honestly is a NEW peer's join snapshot (sendAwareness on
// upgrade, EncodeUpdate(nil) of whatever the room currently holds) — so C
// joins only AFTER several expiry-sweep ticks have had the chance to act,
// and its snapshot is what this test actually checks.
func TestClient_Awareness_HeartbeatSurvivesServerExpiry(t *testing.T) {
	const awarenessExpiry = 200 * time.Millisecond
	_, ts := startServer(t, func(s *ygws.Server) { s.AwarenessExpiry = awarenessExpiry })
	const room = "awareness-heartbeat"

	const pingInterval = 50 * time.Millisecond // well under AwarenessExpiry/2's sweep tick
	a, _ := dialSynced(t, ts, room, Options{PingInterval: pingInterval})

	a.Awareness().SetLocalState(map[string]any{"name": "a"})

	// Let several expiry-sweep ticks (AwarenessExpiry/2 apart, per
	// StartAutoExpiry's doc) elapse while A does nothing but heartbeat.
	time.Sleep(5 * awarenessExpiry)

	c, _ := dialSynced(t, ts, room, Options{})
	require.Eventually(t, func() bool {
		cs, ok := c.Awareness().GetStates()[a.Awareness().ClientID()]
		return ok && cs.State["name"] == "a"
	}, 5*time.Second, 10*time.Millisecond,
		"a late-joining peer's own join snapshot did not include A's presence; "+
			"A's heartbeat did not keep the room's server-side Awareness alive "+
			"across the expiry window")
}

// TestClient_Awareness_SelfCorrectionEscapesEchoSuppression protects the one
// narrow exception onAwarenessUpdate carries (#165 Task 8 review, Important
// 1b; see that method's doc): Awareness.ApplyUpdate's own self-state
// protection (#73 vector C1) can be triggered by a network-origin update
// that wrongly targets THIS Client's own clientID with a null entry — a
// belated provider/websocket disconnect-triggered removal landing after a
// fast reconnect is one concrete way that happens (see loop.go's
// handshake-completion comment). ApplyUpdate corrects its own in-memory
// state immediately, but reports the correction with Origin still set to
// c.remoteOrigin (from onAwarenessUpdate's point of view, this update DID
// arrive over the wire). Without this exception, the correction would never
// reach the wire, and every peer that already accepted the bad removal
// would keep believing this Client is gone for up to a full PingInterval.
//
// A hand-rolled raw server is used so it can inject the poisoned entry
// directly and then watch for the resulting re-announcement — nothing
// alive in ygo's own server actually manufactures a wrongly-targeted
// removal like this without the belated-disconnect race this test is
// standing in for.
func TestClient_Awareness_SelfCorrectionEscapesEchoSuppression(t *testing.T) {
	doc := crdt.New()
	ownID := uint64(doc.ClientID())
	const poisonClock = uint64(5) // comfortably past the clock a lone SetLocalState call produces (1)

	upgrader := gws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	correctionCh := make(chan map[string]any, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, err := conn.ReadMessage(); err != nil { // drain SyncStep1
			return
		}

		// The poison: a null entry targeting ownID at a clock well past
		// what this Client has published, mirroring the shape (if not the
		// exact origin) of a belated disconnect-triggered removal.
		poison := encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarUint(1)
			enc.WriteVarUint(ownID)
			enc.WriteVarUint(poisonClock)
			enc.WriteVarString("null")
		})
		frame := encodeEnvelope(wireMsgAwareness, encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarBytes(poison)
		}))
		if err := conn.WriteMessage(gws.BinaryMessage, frame); err != nil {
			return
		}

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msgType, payload, err := decodeEnvelope(data)
			if err != nil || msgType != wireMsgAwareness {
				continue
			}
			awBytes, err := encoding.NewDecoder(payload).ReadVarBytes()
			if err != nil {
				continue
			}
			scratch := awareness.New(0)
			if err := scratch.ApplyUpdate(awBytes, nil); err != nil {
				continue
			}
			cs, ok := scratch.GetStates()[ownID]
			// Skip anything at or below poisonClock: that is either this
			// Client's ORIGINAL pre-poison announcement (clock 1) or the
			// poison being read back, neither of which proves a
			// correction was sent. Only a clock strictly greater than the
			// poison's can be the self-correction under test.
			if !ok || cs.Clock <= poisonClock {
				continue
			}
			correctionCh <- cs.State
			return
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c, err := New(Options{
		URL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/self-correction",
		Doc: doc,
	})
	require.NoError(t, err)
	c.Awareness().SetLocalState(map[string]any{"name": "c"})
	connect(t, c)

	select {
	case state := <-correctionCh:
		require.Equal(t, "c", state["name"])
	case <-time.After(5 * time.Second):
		t.Fatal("client never re-announced its own state after a network-origin " +
			"update wrongly targeted its own clientID with a removal")
	}
}

// TestClient_Awareness_DropsRemoteStateOnDisconnect_WithoutLeakingTombstones
// covers two required behaviours together because they are two ends of the
// SAME mechanism (#165 Task 8 review, Important 3 + Important 4;
// dropRemoteAwareness):
//
//  1. A remote peer's state must actually disappear from THIS Client's own
//     Awareness once the connection carrying it is lost — the brief's
//     disconnect requirement, previously unexercised: deleting
//     dropRemoteAwareness's call site (loop.go's runLoop) left every other
//     test in this file passing.
//  2. The manufactured tombstone dropRemoteAwareness leaves behind must
//     never leak back out: neither unprompted on the next connection
//     (dropRemoteAwareness's own "must not re-broadcast" design, verified
//     here rather than assumed) nor in answer to a queryAwareness probe —
//     loop.go's wireMsgQueryAwareness case answers with only this Client's
//     own clientID specifically so a manufactured tombstone about someone
//     ELSE is never handed to a peer that asks.
//
// A hand-rolled raw server is used (not ygo's own provider/websocket.Server)
// so the FIRST connection can inject a synthetic remote clientID directly,
// and the SECOND (the client's own automatic reconnect) can inspect exactly
// what the client sends unprompted and in reply to an explicit query —
// neither of which a real server's own handshake sequence would let this
// test isolate.
func TestClient_Awareness_DropsRemoteStateOnDisconnect_WithoutLeakingTombstones(t *testing.T) {
	const ghostID = uint64(999)

	upgrader := gws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connNum atomic.Int32
	ghostAnnounced := make(chan struct{})
	closeFirst := make(chan struct{})
	secondConnLeaked := make(chan bool, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		if connNum.Add(1) == 1 {
			if _, _, err := conn.ReadMessage(); err != nil { // drain SyncStep1
				return
			}
			ghost := encoding.EncodeBytes(func(enc *encoding.Encoder) {
				enc.WriteVarUint(1)
				enc.WriteVarUint(ghostID)
				enc.WriteVarUint(uint64(1))
				enc.WriteVarString(`{"name":"ghost"}`)
			})
			frame := encodeEnvelope(wireMsgAwareness, encoding.EncodeBytes(func(enc *encoding.Encoder) {
				enc.WriteVarBytes(ghost)
			}))
			if err := conn.WriteMessage(gws.BinaryMessage, frame); err != nil {
				return
			}
			close(ghostAnnounced)
			<-closeFirst // hold the connection open until the test is ready for it to die
			return
		}

		// Second (and every later) connection: drain SyncStep1, explicitly
		// query what the client knows, then collect everything it sends —
		// both the query reply and anything unprompted — for a bounded
		// window.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(gws.BinaryMessage, encodeEnvelope(wireMsgQueryAwareness, nil)); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		leaked := false
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			msgType, payload, err := decodeEnvelope(data)
			if err != nil || msgType != wireMsgAwareness {
				continue
			}
			awBytes, err := encoding.NewDecoder(payload).ReadVarBytes()
			if err != nil {
				continue
			}
			d := encoding.NewDecoder(awBytes)
			cnt, err := d.ReadVarUint()
			if err != nil {
				continue
			}
			for i := uint64(0); i < cnt; i++ {
				cid, err := d.ReadVarUint()
				if err != nil {
					break
				}
				if _, err := d.ReadVarUint(); err != nil { // clock
					break
				}
				if _, err := d.ReadVarBytes(); err != nil { // state JSON
					break
				}
				if cid == ghostID {
					leaked = true
				}
			}
		}
		select {
		case secondConnLeaked <- leaked:
		default:
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c, err := New(Options{
		URL:        "ws" + strings.TrimPrefix(ts.URL, "http") + "/drop-remote",
		Doc:        crdt.New(),
		MaxBackoff: 500 * time.Millisecond,
	})
	require.NoError(t, err)
	connect(t, c)

	select {
	case <-ghostAnnounced:
	case <-time.After(5 * time.Second):
		t.Fatal("first connection's synthetic remote clientID was never sent")
	}
	require.Eventually(t, func() bool {
		_, ok := c.Awareness().GetStates()[ghostID]
		return ok
	}, 2*time.Second, 10*time.Millisecond, "client never applied the synthetic remote clientID")

	disc := statusWaiter(t, c, StateDisconnected)
	close(closeFirst)
	disc()

	require.Eventually(t, func() bool {
		_, ok := c.Awareness().GetStates()[ghostID]
		return !ok
	}, 2*time.Second, 10*time.Millisecond,
		"ghost clientID was not dropped from this Client's own Awareness after disconnect")

	select {
	case leaked := <-secondConnLeaked:
		require.False(t, leaked,
			"reconnect leaked the dropped ghost clientID, either unprompted or in a queryAwareness reply")
	case <-time.After(5 * time.Second):
		t.Fatal("second connection never completed its read window")
	}
}

// TestClient_Awareness_CallerDrivenExpiryDoesNotRebroadcastOtherPeers is the
// final whole-branch review's Important C: Client.Awareness's own godoc
// (see its "propagates to the server" paragraph) says local state set on the
// returned *awareness.Awareness propagates via onAwarenessUpdate regardless
// of whether Connect has been called — which is true, but onAwarenessUpdate
// used to forward EVERY clientID in ANY non-remoteOrigin UpdateEvent, not
// just this Client's own. awareness.Awareness.RemoveExpired (and, by the
// same mechanism, StartAutoExpiry, which the awareness package's own doc
// recommends a caller wire up) fires with a nil Origin — never
// c.remoteOrigin — and its Removed set is always some OTHER peer's
// clientID (RemoveExpired never self-expires the local client; see its own
// doc). So a caller driving expiry on Client.Awareness() the documented way
// made this Client encode and queue a null entry for a peer it does not
// own, which a real server would broadcast room-wide, evicting a peer that
// may still be perfectly present — the same interop hazard an earlier round
// fixed for queryAwareness (see loop.go's wireMsgQueryAwareness case),
// reached through a different door.
//
// This is deliberately network-free: nothing here calls Connect, so the
// lane is never drained by anything else, and c.lane.Empty() after
// RemoveExpired is an unambiguous, race-free proof of whether a push
// happened — an end-to-end version through a live connection would have to
// race the loop's own flushLane call to observe the same thing.
func TestClient_Awareness_CallerDrivenExpiryDoesNotRebroadcastOtherPeers(t *testing.T) {
	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: doc})
	require.NoError(t, err)

	// Simulate a remote peer's presence having already arrived over the
	// network — applied under c.remoteOrigin, exactly as handleFrame's
	// wireMsgAwareness case does for a real inbound frame.
	remoteID := c.awareness.ClientID() + 1
	remoteJoin := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1)
		enc.WriteVarUint(remoteID)
		enc.WriteVarUint(uint64(1))
		enc.WriteVarString(`{"name":"peer"}`)
	})
	require.NoError(t, c.awareness.ApplyUpdate(remoteJoin, c.remoteOrigin))
	require.True(t, c.lane.Empty(),
		"the remote peer's own arrival must not have queued anything (it is neither this Client's "+
			"own state nor a self-correction) — precondition for the assertion below")

	// Caller-driven expiry directly on this Client's own Awareness — the
	// exact pattern Awareness.RemoveExpired's doc recommends (StartAutoExpiry
	// automates the same call). timeout=0 expires the remote peer
	// immediately; RemoveExpired never self-expires the local clientID, so
	// remoteID is the only entry that can possibly be removed here.
	c.awareness.RemoveExpired(0)

	require.True(t, c.lane.Empty(),
		"RemoveExpired removing an OTHER peer's presence must not queue an outbound announcement — "+
			"this Client does not own that peer's state, and re-broadcasting its removal would let a "+
			"real server evict a peer that is still present everywhere else (#165 final review, Important C)")
}
