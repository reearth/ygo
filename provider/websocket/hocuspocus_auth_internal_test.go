// Package websocket - internal test for Hocuspocus docName framing (#104).
//
// This lives in the internal (package websocket) test set, rather than in
// hocuspocus_test.go (package websocket_test), because the external test
// package cannot see unexported identifiers such as peer.hocuspocusFraming
// or the dial/readOne helpers used to build this test's local equivalents.
package websocket

import (
	"context"
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
