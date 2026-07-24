// Package websocket - internal test for Hocuspocus docName framing (#104).
//
// This lives in the internal (package websocket) test set, rather than in
// hocuspocus_test.go (package websocket_test), because the external test
// package cannot see unexported identifiers such as peer.hocuspocusFraming
// or the dial/readOne helpers used to build this test's local equivalents.
package websocket

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// dialRoomInternal opens a WebSocket connection to the test server at
// "/"+room, mirroring wsURL/dial in server_test.go (package websocket_test)
// which cannot be reused here because it lives in a different package.
func dialRoomInternal(t *testing.T, ts *httptest.Server, room string) *gws.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + room
	conn, _, err := gws.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestHocuspocusFraming_RoundTrip verifies that with HocuspocusFraming
// enabled, every server->client frame is prefixed with VarString(roomName)
// as Hocuspocus's own wire protocol requires (#104).
func TestHocuspocusFraming_RoundTrip(t *testing.T) {
	// NewServer (not a bare &Server{}) so rooms/upgrader/shutdownCh are
	// initialized — ServeHTTP writes into s.rooms and Shutdown closes
	// s.shutdownCh, both nil on the zero value.
	srv := NewServer()
	srv.HocuspocusFraming = true
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	room := "doc-room"
	c := dialRoomInternal(t, ts, room)
	defer c.Close()

	// First server->client frame must be docName-prefixed.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Time{})

	dec := encoding.NewDecoder(data)
	name, err := dec.ReadVarString()
	require.NoError(t, err)
	require.Equal(t, room, name, "server frames are docName-prefixed")
	_, err = dec.ReadVarUint() // a valid message tag follows
	require.NoError(t, err)
}

// TestInbandAuth_Authenticated_ReadWrite verifies the success path of the
// Hocuspocus in-band Auth (tag 2) sub-protocol: a Server.OnTokenAuth hook that
// accepts the token results in an Authenticated (sub-type 2) reply carrying
// the "read-write" scope, and the hook receives the correct room + token
// (#104).
func TestInbandAuth_Authenticated_ReadWrite(t *testing.T) {
	var gotRoom, gotToken string
	// NewServer (not a bare &Server{}) so rooms/upgrader/shutdownCh are
	// initialized — ServeHTTP writes into s.rooms and Shutdown closes
	// s.shutdownCh, both nil on the zero value.
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(room, token string) (ConnectionConfig, error) {
		gotRoom, gotToken = room, token
		return ConnectionConfig{ReadOnly: false}, nil
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	room := "r1"
	c := dialRoomInternal(t, ts, room)
	defer c.Close()

	// Send Auth: VarString(docName) VarUint(2) VarUint(0=Token) VarString(token)
	frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString("secret-token")
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, frame))

	// Expect an Authenticated reply somewhere in the incoming frames.
	require.Eventually(t, func() bool {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, data, err := c.ReadMessage()
		if err != nil {
			return false
		}
		dec := encoding.NewDecoder(data)
		_, _ = dec.ReadVarString() // docName
		tag, _ := dec.ReadVarUint()
		if tag != msgAuth {
			return false
		}
		sub, _ := dec.ReadVarUint()
		scope, _ := dec.ReadVarString()
		return sub == authTypeAuthenticated && scope == "read-write"
	}, 2*time.Second, 20*time.Millisecond)

	require.Equal(t, room, gotRoom)
	require.Equal(t, "secret-token", gotToken)
}

// TestInbandAuth_PermissionDenied_Then4401 verifies the G1 fix (#104): a
// PermissionDenied (tag msgAuth, sub authTypePermissionDenied) data frame must
// be observed by the client BEFORE the connection closes, and the close must
// carry the Hocuspocus 4401 (wsCodeUnauthorized) code. Both the data frame and
// the close are funneled through the single per-peer writer goroutine via
// enqueueClose's nil-sentinel, so ordering is guaranteed even though the tag-2
// auth handler runs on the read-loop goroutine.
func TestInbandAuth_PermissionDenied_Then4401(t *testing.T) {
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(room, token string) (ConnectionConfig, error) {
		return ConnectionConfig{}, fmt.Errorf("bad token")
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	room := "r1"
	c := dialRoomInternal(t, ts, room)
	defer c.Close()

	frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString("nope")
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, frame))

	// Read frames until the connection closes; we must observe the
	// PermissionDenied data frame before the close error.
	sawDenied := false
	var closeErr error
	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			closeErr = err
			break
		}
		dec := encoding.NewDecoder(data)
		_, _ = dec.ReadVarString() // docName
		if tag, _ := dec.ReadVarUint(); tag == msgAuth {
			if sub, _ := dec.ReadVarUint(); sub == authTypePermissionDenied {
				sawDenied = true
			}
		}
	}
	require.True(t, sawDenied, "PermissionDenied must arrive before close")
	require.True(t, gws.IsCloseError(closeErr, wsCodeUnauthorized),
		"connection must close with 4401, got %v", closeErr)
}

// TestInbandAuth_LongErrorReason_StillCloses4401 guards the fix for the WS
// control-frame 125-byte cap: an OnTokenAuth error longer than the close-frame
// payload limit must NOT drop the 4401 close. The full error text still rides
// in the PermissionDenied data frame; the close frame uses the short constant
// reason, so the client always sees the 4401 code.
func TestInbandAuth_LongErrorReason_StillCloses4401(t *testing.T) {
	longReason := strings.Repeat("x", 300) // >123 bytes: would overflow a close frame
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(room, token string) (ConnectionConfig, error) {
		return ConnectionConfig{}, fmt.Errorf("%s", longReason)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	room := "r1"
	c := dialRoomInternal(t, ts, room)
	defer c.Close()

	frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString("nope")
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, frame))

	sawFullReason := false
	var closeErr error
	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			closeErr = err
			break
		}
		dec := encoding.NewDecoder(data)
		_, _ = dec.ReadVarString() // docName
		if tag, _ := dec.ReadVarUint(); tag == msgAuth {
			if sub, _ := dec.ReadVarUint(); sub == authTypePermissionDenied {
				reason, _ := dec.ReadVarString()
				if reason == longReason {
					sawFullReason = true
				}
			}
		}
	}
	require.True(t, sawFullReason, "full error text must survive in the PermissionDenied data frame")
	require.True(t, gws.IsCloseError(closeErr, wsCodeUnauthorized),
		"4401 close must still be delivered despite a long hook error, got %v", closeErr)
}

// TestInbandAuth_NilHook_Ignored verifies backward compatibility (#104): when
// Server.OnTokenAuth is nil, an inbound Auth (tag 2) frame is silently
// ignored — no reply, no close — and the connection keeps working normally,
// matching the legacy y-websocket behavior where tag 2 has no handler at all.
func TestInbandAuth_NilHook_Ignored(t *testing.T) {
	srv := NewServer()
	srv.HocuspocusFraming = true
	// OnTokenAuth intentionally left nil.
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	room := "r1"
	c := dialRoomInternal(t, ts, room)
	defer c.Close()

	frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString("whatever")
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, frame))

	// Connection must stay open (no auth reply, no close): a follow-up
	// QueryAwareness still gets answered.
	q := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgQueryAwareness)
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, q))

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c.ReadMessage()
	require.NoError(t, err, "connection stays open with nil OnTokenAuth")
}

// TestInbandAuth_ReadonlyScope verifies that an OnTokenAuth hook returning
// ConnectionConfig{ReadOnly: true} results in an Authenticated (sub-type 2)
// reply carrying the "readonly" scope string (#104).
func TestInbandAuth_ReadonlyScope(t *testing.T) {
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(room, token string) (ConnectionConfig, error) {
		return ConnectionConfig{ReadOnly: true}, nil
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	room := "r1"
	c := dialRoomInternal(t, ts, room)
	defer c.Close()

	frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString("t")
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, frame))

	require.Eventually(t, func() bool {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, data, err := c.ReadMessage()
		if err != nil {
			return false
		}
		dec := encoding.NewDecoder(data)
		_, _ = dec.ReadVarString() // docName
		if tag, _ := dec.ReadVarUint(); tag != msgAuth {
			return false
		}
		sub, _ := dec.ReadVarUint()
		scope, _ := dec.ReadVarString()
		return sub == authTypeAuthenticated && scope == "readonly"
	}, 2*time.Second, 20*time.Millisecond)
}

// TestInbandAuth_HookPanic_Denied verifies that a panicking OnTokenAuth hook
// is converted into a denial by safeTokenAuth's recover, rather than
// crashing the read-loop goroutine: the connection must be closed (a
// PermissionDenied frame followed by a close, or at minimum a read error),
// never left silently open or the process taken down (#104 Task 7 review).
func TestInbandAuth_HookPanic_Denied(t *testing.T) {
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(room, token string) (ConnectionConfig, error) {
		panic("boom")
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	room := "r1"
	c := dialRoomInternal(t, ts, room)
	defer c.Close()

	frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString("nope")
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, frame))

	// Read frames until the connection closes; a panicking hook must never
	// leave the connection silently open. A PermissionDenied data frame may
	// or may not be observed depending on timing, but a close must follow.
	var closeErr error
	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, err := c.ReadMessage()
		if err != nil {
			closeErr = err
			break
		}
	}
	require.Error(t, closeErr, "connection must close after a panicking OnTokenAuth hook")
	require.True(t, gws.IsCloseError(closeErr, wsCodeUnauthorized),
		"connection must close with 4401 after a panicking hook, got %v", closeErr)
}

// TestInbandAuth_WrongSubType_Ignored verifies that an Auth frame carrying a
// sub-type other than Token(0) — e.g. Authenticated(2), a server->client-only
// sub-type — is ignored by the server: no reply, no close, and the
// connection keeps working (#104 Task 7 review).
func TestInbandAuth_WrongSubType_Ignored(t *testing.T) {
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(room, token string) (ConnectionConfig, error) {
		return ConnectionConfig{ReadOnly: false}, nil
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	room := "r1"
	c := dialRoomInternal(t, ts, room)
	defer c.Close()

	frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeAuthenticated) // wrong sub-type: not Token(0)
		enc.WriteVarString("irrelevant")
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, frame))

	// Connection must stay open: a follow-up QueryAwareness still gets
	// answered, and it must not be an Auth reply.
	q := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgQueryAwareness)
	})
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, q))

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	require.NoError(t, err, "connection stays open after a non-Token Auth sub-type")
	dec := encoding.NewDecoder(data)
	_, _ = dec.ReadVarString() // docName
	tag, _ := dec.ReadVarUint()
	require.NotEqual(t, msgAuth, tag, "wrong sub-type Auth frame must not produce an Auth reply")
}
