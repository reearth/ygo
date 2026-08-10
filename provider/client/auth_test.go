package client

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// This file exercises #165 Task 9: the Hocuspocus in-band token auth
// exchange (mirroring provider/websocket's #104 OnTokenAuth surface, read
// from provider/websocket/hocuspocus_auth_internal_test.go and peer.go
// before writing anything here — see that test file for the exact frame
// shapes and provider/client/wire.go for how this package's constants pin
// against them).
//
// Deliberately NOT exercised here: HocuspocusFraming (the docName-prefixed
// wire variant). This client only ever speaks the plain, non-docName-prefixed
// framing described in wire.go/loop.go, for the Auth (tag 2) exchange same
// as everything else — see the design doc's "two shipped framings only"
// non-goal. provider/websocket's OnTokenAuth hook does not require
// HocuspocusFraming to be enabled (handleAuth does not consult
// p.hocuspocusFraming at all — only handleMessage's docName-prefix read/write
// does), so leaving it at its default false on every server this file starts
// is intentional, not an oversight.

// TestClient_Auth_CorrectTokenSyncs is behaviour (a): a client configured
// with the correct Options.Token completes the handshake and syncs, exactly
// like a client with no Token at all — the auth exchange rides alongside the
// handshake, it does not replace or block it (see provider/websocket's
// OnTokenAuth doc: "the initial sync is served before any PermissionDenied",
// i.e. ygo's own server never gates the handshake on auth in the first
// place).
func TestClient_Auth_CorrectTokenSyncs(t *testing.T) {
	// authDone establishes a happens-before edge between the server's
	// OnTokenAuth goroutine writing gotRoom/gotToken and this test's
	// goroutine reading them below. Synced() closing is NOT enough on its
	// own: ygo's own server pushes SyncStep1/Step2 (what Synced() actually
	// waits on) independently of — and, per its own doc, before — anything
	// to do with auth, so a client can legitimately reach StateSynced before
	// OnTokenAuth has even been invoked. Racing that read against the
	// hook's write (rather than synchronizing on it) is exactly what
	// `-race` catches without this channel.
	authDone := make(chan struct{})
	var gotRoom, gotToken string
	srv, ts := startServer(t, func(s *ygws.Server) {
		s.OnTokenAuth = func(room, token string) (ygws.ConnectionConfig, error) {
			gotRoom, gotToken = room, token
			close(authDone)
			return ygws.ConnectionConfig{}, nil
		}
	})
	const room = "auth-ok"

	c, doc := dialSynced(t, ts, room, Options{Token: "correct-token"})
	_ = c

	select {
	case <-authDone:
	case <-time.After(2 * time.Second):
		t.Fatal("OnTokenAuth was never invoked; the client never sent its Auth token frame")
	}
	require.Equal(t, room, gotRoom, "OnTokenAuth must see the room this client connected to")
	require.Equal(t, "correct-token", gotToken, "OnTokenAuth must see exactly the token this client sent")

	// The connection is genuinely live post-auth, not merely upgraded and
	// then stuck: a local edit made now must still reach the server, proving
	// the auth exchange did not leave the sync loop in some half-handshaken
	// state that only looked synced.
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hello", nil) })
	require.Eventually(t, func() bool {
		d := srv.GetDoc(room)
		return d != nil && d.GetText("t").ToString() == "hello"
	}, 5*time.Second, 10*time.Millisecond, "a post-auth edit never reached the server")
}

// TestClient_Auth_WrongTokenIsTerminal is behaviour (b), the load-bearing
// design point of this task: an auth rejection is TERMINAL, not retryable.
// runReconnectLoop's ordinary contract is to back off and dial again
// forever — correct for a flaky network, catastrophic for a bad token that
// will never start working no matter how many times it is retried. This
// test proves both halves at once: Connect returns promptly with the
// sentinel error (does not spin forever inside runReconnectLoop's select),
// AND the server-side OnTokenAuth hook — which fires once per connection
// attempt — is never invoked a second time, ruling out a retry that merely
// happens to be slow rather than one that never starts.
//
// The count assertion below is what actually proves terminality, not a
// wall-clock bound on how fast Connect returned. An earlier version of this
// test also asserted elapsed < 300ms, on the theory that a terminal
// rejection returns "on the first attempt" and so must be fast; that flaked
// on a loaded GitHub Actions runner (elapsed observed at 358ms) even though
// the client had genuinely never retried — a slow machine makes Connect
// itself slower to return without that being evidence of a retry. The
// authCalls count has no such failure mode: a slow machine only makes a
// would-be retry LESS likely to land inside the wait window below, never
// more, so the count staying at 1 is unambiguous positive proof that no
// second attempt happened, regardless of how long the first one took.
func TestClient_Auth_WrongTokenIsTerminal(t *testing.T) {
	var authCalls atomic.Int32
	_, ts := startServer(t, func(s *ygws.Server) {
		s.OnTokenAuth = func(room, token string) (ygws.ConnectionConfig, error) {
			authCalls.Add(1)
			return ygws.ConnectionConfig{}, fmt.Errorf("bad token")
		}
	})
	const room = "auth-bad"

	c, err := New(Options{
		URL:   wsURL(ts, room),
		Doc:   crdt.New(),
		Token: "wrong-token",
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()

	// This 5s bound's only job is to fail a genuine hang (Connect spinning
	// forever inside runReconnectLoop's select instead of returning at all);
	// it is deliberately generous rather than tight, unlike the wall-clock
	// bound this test used to also assert — see this test's own doc above.
	var connectErr error
	select {
	case connectErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return for a rejected token; it appears to be retrying forever " +
			"instead of treating the rejection as terminal")
	}

	require.ErrorIs(t, connectErr, ErrAuthRejected,
		"Connect's returned error must satisfy errors.Is(err, ErrAuthRejected) so callers can branch on it")

	// reconnectBackoffBase alone is 500ms, so ~2s comfortably exceeds several
	// backoff cycles' worth of waiting. If the reconnect loop had fed this
	// rejection back into itself instead of stopping, it would have dialed
	// and been rejected again well within this window. authCalls staying at
	// exactly 1 is the positive proof that no such retry exists.
	time.Sleep(2 * time.Second)
	require.Equal(t, int32(1), authCalls.Load(),
		"OnTokenAuth was invoked more than once: the reconnect loop retried a rejected token "+
			"against the server instead of stopping after the first rejection")
}

// TestClient_Auth_EmptyTokenSendsNoAuthFrame is behaviour (c): with
// Options.Token left at its zero value, the wire is byte-identical to
// today's plain y-websocket flow — no Auth frame, ever. This is proved by
// observation on a raw, hand-rolled WebSocket server (not ygo's own
// provider/websocket.Server, and not by reading wire.go/loop.go's source):
// the only thing this client sends unprompted at connect is the SyncStep1
// envelope, and nothing else follows it before the server has said anything
// back.
func TestClient_Auth_EmptyTokenSendsNoAuthFrame(t *testing.T) {
	upgrader := gws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	frames := make(chan []byte, 8)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case frames <- data:
			default:
			}
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c, err := New(Options{
		URL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/no-auth",
		Doc: crdt.New(),
		// Token deliberately left at its zero value (""): the whole point
		// of this test.
	})
	require.NoError(t, err)
	connect(t, c)

	var first []byte
	select {
	case first = <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("client never sent its initial SyncStep1 frame")
	}
	typ, payload, err := decodeEnvelope(first)
	require.NoError(t, err)
	require.Equal(t, wireMsgSync, typ,
		"the only unprompted frame a Token==\"\" client sends must be Sync (SyncStep1), never Auth")
	require.True(t, bytes.HasPrefix(payload, []byte{0}) || len(payload) > 0,
		"sanity: SyncStep1 payload should be non-empty")

	select {
	case second := <-frames:
		typ2, _, _ := decodeEnvelope(second)
		t.Fatalf("unexpected second frame observed on the wire (msgType=%d): Token==\"\" must "+
			"produce exactly the plain y-websocket handshake, with no auth frame sent before the "+
			"server has replied to anything", typ2)
	case <-time.After(200 * time.Millisecond):
		// No second frame arrived: exactly today's plain y-websocket
		// behaviour, proved by observation rather than by inspecting the
		// code that produces it.
	}
}

// TestAuth_ConstantsMatchServer pins authTypeToken/authTypePermissionDenied/
// authTypeAuthenticated to provider/websocket/server.go's identically-named
// constants byte-for-byte, the same way wire_test.go's
// TestEnvelope_ConstantsMatchServer already pins the outer envelope tags —
// this client's Auth (tag 2) sub-protocol must agree with the server's on
// these exact values to interoperate (#104, #165).
func TestAuth_ConstantsMatchServer(t *testing.T) {
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"authTypeToken", authTypeToken, 0},
		{"authTypePermissionDenied", authTypePermissionDenied, 1},
		{"authTypeAuthenticated", authTypeAuthenticated, 2},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestEncodeAuthToken_DecodeAuthReply_RoundTrip is a small unit-level check
// on the two codec functions this task adds, independent of any live
// connection: encodeAuthToken must produce a frame whose outer envelope tag
// is wireMsgAuth and whose inner payload decodeAuthReply can parse back out
// again — specifically the shape provider/websocket/peer.go's handleAuth
// reads (VarUint(subType) VarString(s)), which is what decodeAuthReply is
// ALSO used against for a server's reply (a different subType/string pair,
// same shape).
func TestEncodeAuthToken_DecodeAuthReply_RoundTrip(t *testing.T) {
	frame := encodeAuthToken("my-token")

	typ, payload, err := decodeEnvelope(frame)
	require.NoError(t, err)
	require.Equal(t, wireMsgAuth, typ)

	subType, s, err := decodeAuthReply(payload)
	require.NoError(t, err)
	require.Equal(t, authTypeToken, subType)
	require.Equal(t, "my-token", s)
}

// TestDecodeAuthReply_MatchesServerEncoding cross-checks decodeAuthReply
// against provider/websocket/peer.go's actual encodeAuthMessage output shape
// (VarUint(msgAuth) VarUint(subType) VarString(s)) built by hand here, since
// encodeAuthMessage itself is unexported and lives in a different package.
// This is the client-side half of the same wire agreement
// hocuspocus_auth_internal_test.go verifies server-side.
func TestDecodeAuthReply_MatchesServerEncoding(t *testing.T) {
	serverFrame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(2)) // msgAuth, hand-written to avoid importing the server's unexported constant
		enc.WriteVarUint(authTypeAuthenticated)
		enc.WriteVarString("read-write")
	})

	typ, payload, err := decodeEnvelope(serverFrame)
	require.NoError(t, err)
	require.Equal(t, wireMsgAuth, typ)

	subType, scope, err := decodeAuthReply(payload)
	require.NoError(t, err)
	require.Equal(t, authTypeAuthenticated, subType)
	require.Equal(t, "read-write", scope)
}
