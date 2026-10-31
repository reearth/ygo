package websocket

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// connectionKey carries a test connection's name in its request context, as an
// embedder marks each connection it serves.
type connectionKey struct{}

// serveNamed serves srv so that each connection's request context carries the
// value of its "name" query parameter under connectionKey.
func serveNamed(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), connectionKey{}, r.URL.Query().Get("name"))
		srv.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return ts
}

// sendToken sends a Hocuspocus Auth Token frame for room.
func sendToken(t *testing.T, c *gws.Conn, room, token string) {
	t.Helper()
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString(token)
	})))
}

// frameTag returns a frame's outer tag and a decoder positioned after it.
func frameTag(data []byte) (uint64, *encoding.Decoder) {
	dec := encoding.NewDecoder(data)
	_, _ = dec.ReadVarString() // docName
	tag, _ := dec.ReadVarUint()
	return tag, dec
}

// readTag reads c until a frame with the given outer tag, and returns a
// decoder positioned after the tag, or the read error.
func readTag(t *testing.T, c *gws.Conn, tag uint64) (*encoding.Decoder, error) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return nil, err
		}
		if got, dec := frameTag(data); got == tag {
			return dec, nil
		}
	}
}

// readAuthReply reads c until an Auth reply or the end of the connection, and
// returns the reply's sub-type and string, or the read error.
func readAuthReply(t *testing.T, c *gws.Conn) (uint64, string, error) {
	t.Helper()
	dec, err := readTag(t, c, msgAuth)
	if err != nil {
		return 0, "", err
	}
	sub, _ := dec.ReadVarUint()
	text, _ := dec.ReadVarString()
	return sub, text, nil
}

// sendWrite sends an update that sets key in the "writes" map, then waits for
// the reply to a QueryAwareness sent after it, so the server has handled the
// update when it returns. It returns the text of each Auth reply c read
// before that reply.
func sendWrite(t *testing.T, c *gws.Conn, room, key string, client crdt.ClientID) []string {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(client))
	m := doc.GetMap("writes")
	doc.Transact(func(txn *crdt.Transaction) { m.Set(txn, key, true) })
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgSync)
		enc.WriteRaw(ygsync.EncodeUpdate(crdt.EncodeStateAsUpdateV1(doc, nil)))
	})))
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString(room)
		enc.WriteVarUint(msgQueryAwareness)
	})))
	require.NoError(t, c.SetReadDeadline(time.Now().Add(2*time.Second)))
	var authReplies []string
	for {
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		switch tag, dec := frameTag(data); tag {
		case msgAwareness:
			return authReplies
		case msgAuth:
			_, _ = dec.ReadVarUint() // sub-type
			text, _ := dec.ReadVarString()
			authReplies = append(authReplies, text)
		}
	}
}

// Two connections to one room send the same token, the revoked one after the
// live one was answered. OnTokenAuthContext tells them apart by their request
// contexts, so the revoked connection's late answer is its own: it is read-only,
// and the live connection stays read-write and keeps writing.
func TestUnit_Server_OnTokenAuthContext_ALateTokenIsAnsweredOnItsOwnConnection(t *testing.T) {
	const room, token = "r1", "shared-token"
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuthContext = func(ctx context.Context, gotRoom, gotToken string) (ConnectionConfig, error) {
		if gotRoom != room || gotToken != token {
			return ConnectionConfig{}, errors.New("unexpected room or token")
		}
		return ConnectionConfig{ReadOnly: ctx.Value(connectionKey{}) == "revoked"}, nil
	}
	ts := serveNamed(t, srv)

	live := dialRoomInternal(t, ts, room+"?name=live")
	revoked := dialRoomInternal(t, ts, room+"?name=revoked")

	sendToken(t, live, room, token)
	sub, scope, err := readAuthReply(t, live)
	require.NoError(t, err)
	require.Equal(t, authTypeAuthenticated, sub)
	require.Equal(t, "read-write", scope)

	sendToken(t, revoked, room, token)
	sub, scope, err = readAuthReply(t, revoked)
	require.NoError(t, err)
	require.Equal(t, authTypeAuthenticated, sub)
	require.Equal(t, "readonly", scope)

	sendWrite(t, revoked, room, "revoked", 11)
	assert.Empty(t, sendWrite(t, live, room, "live", 12),
		"late auth reply crossed sockets: the live connection got another connection's answer")
	assert.ElementsMatch(t, []string{"live"}, srv.GetDoc(room).GetMap("writes").Keys(),
		"only the live connection's write reaches the room")
}

// A late answer reaches only the connection that asked, not a replacement in
// the room that sent the same token (a reply-broadcast overlay fails here).
func TestUnit_Server_OnTokenAuthContext_ALateAnswerDoesNotReachAReplacement(t *testing.T) {
	const room, token = "r1", "shared-token"
	oldAsked, release := make(chan struct{}), make(chan struct{})
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuthContext = func(ctx context.Context, gotRoom, gotToken string) (ConnectionConfig, error) {
		if gotRoom != room || gotToken != token {
			return ConnectionConfig{}, errors.New("unexpected room or token")
		}
		if ctx.Value(connectionKey{}) != "old" {
			return ConnectionConfig{}, nil
		}
		close(oldAsked)
		select { // bounded, as the hook's doc asks
		case <-release: // the embedder revoked the old connection
			return ConnectionConfig{ReadOnly: true}, nil
		case <-time.After(5 * time.Second):
			return ConnectionConfig{}, errors.New("the test never released the hook")
		}
	}
	ts := serveNamed(t, srv)

	old := dialRoomInternal(t, ts, room+"?name=old")
	sendToken(t, old, room, token)
	select {
	case <-oldAsked:
	case <-time.After(2 * time.Second):
		t.Fatal("no hook call carried the old connection's context")
	}

	replacement := dialRoomInternal(t, ts, room+"?name=replacement")
	sendToken(t, replacement, room, token)
	sub, scope, err := readAuthReply(t, replacement)
	require.NoError(t, err)
	require.Equal(t, authTypeAuthenticated, sub)
	require.Equal(t, "read-write", scope)

	close(release)
	sub, scope, err = readAuthReply(t, old)
	require.NoError(t, err, "the old connection got no answer to its token; it may have gone to another connection")
	require.Equal(t, authTypeAuthenticated, sub)
	require.Equal(t, "readonly", scope)
	// The old connection's read loop handles this write after the late
	// answer, so the answer is complete when sendWrite returns.
	sendWrite(t, old, room, "old", 11)

	assert.Empty(t, sendWrite(t, replacement, room, "replacement", 12),
		"late auth reply crossed sockets: the replacement got another connection's answer")
	assert.ElementsMatch(t, []string{"replacement"}, srv.GetDoc(room).GetMap("writes").Keys(),
		"only the replacement's write reaches the room")
}

// OnTokenAuthContext is called in place of OnTokenAuth when both are set.
func TestUnit_Server_OnTokenAuthContext_TakesPrecedence(t *testing.T) {
	srv := NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(string, string) (ConnectionConfig, error) {
		return ConnectionConfig{}, errors.New("OnTokenAuth must not be called")
	}
	gotName := make(chan any, 1)
	srv.OnTokenAuthContext = func(ctx context.Context, _, _ string) (ConnectionConfig, error) {
		gotName <- ctx.Value(connectionKey{})
		return ConnectionConfig{}, nil
	}
	ts := serveNamed(t, srv)

	c := dialRoomInternal(t, ts, "r1?name=first")
	sendToken(t, c, "r1", "t")
	sub, scope, err := readAuthReply(t, c)
	require.NoError(t, err)
	assert.Equal(t, authTypeAuthenticated, sub)
	assert.Equal(t, "read-write", scope)
	select {
	case name := <-gotName: // sent before the reply was written
		assert.Equal(t, "first", name)
	default:
		t.Fatal("OnTokenAuthContext was not called")
	}
}

// Each hook gives the same reply for the same decision: OnTokenAuth alone
// behaves as before OnTokenAuthContext existed, and OnTokenAuthContext follows
// its contract, panics included. A panic's log line names the hook.
func TestUnit_Server_OnTokenAuthContext_RepliesAsOnTokenAuth(t *testing.T) {
	decisions := []struct {
		name      string
		decide    func() (ConnectionConfig, error)
		wantSub   uint64
		wantText  string
		wantClose bool
		wantPanic bool // the hook panics, so the server logs "<hook> panicked; denying"
	}{
		{"read-write", func() (ConnectionConfig, error) { return ConnectionConfig{}, nil }, authTypeAuthenticated, "read-write", false, false},
		{"read-only", func() (ConnectionConfig, error) { return ConnectionConfig{ReadOnly: true}, nil }, authTypeAuthenticated, "readonly", false, false},
		{"denied", func() (ConnectionConfig, error) { return ConnectionConfig{}, errors.New("bad token") }, authTypePermissionDenied, "bad token", true, false},
		{"panic", func() (ConnectionConfig, error) { panic("boom") }, authTypePermissionDenied, "authentication error", true, true},
	}
	hooks := []struct {
		name    string
		install func(*Server, func() (ConnectionConfig, error))
	}{
		{"OnTokenAuth", func(s *Server, decide func() (ConnectionConfig, error)) {
			s.OnTokenAuth = func(string, string) (ConnectionConfig, error) { return decide() }
		}},
		{"OnTokenAuthContext", func(s *Server, decide func() (ConnectionConfig, error)) {
			s.OnTokenAuthContext = func(context.Context, string, string) (ConnectionConfig, error) { return decide() }
		}},
	}
	for _, hook := range hooks {
		for _, d := range decisions {
			t.Run(hook.name+"/"+d.name, func(t *testing.T) {
				srv := NewServer()
				srv.HocuspocusFraming = true
				var logs lockedBuffer
				srv.Logger = slog.New(slog.NewTextHandler(&logs, nil))
				hook.install(srv, d.decide)
				ts := serveNamed(t, srv)

				c := dialRoomInternal(t, ts, "r1")
				sendToken(t, c, "r1", "t")
				sub, text, err := readAuthReply(t, c)
				require.NoError(t, err)
				assert.Equal(t, d.wantSub, sub)
				assert.Equal(t, d.wantText, text)
				if d.wantClose {
					_, _, err = readAuthReply(t, c)
					assert.True(t, gws.IsCloseError(err, wsCodeUnauthorized), "want a 4401 close, got %v", err)
				}
				if d.wantPanic {
					assert.Contains(t, logs.String(), hook.name+" panicked; denying")
				}
			})
		}
	}
}

// lockedBuffer is a bytes.Buffer the server's goroutines and the test can
// share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
