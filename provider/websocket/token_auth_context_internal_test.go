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
		dec := encoding.NewDecoder(data)
		_, _ = dec.ReadVarString() // docName
		if got, _ := dec.ReadVarUint(); got == tag {
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
// update when it returns.
func sendWrite(t *testing.T, c *gws.Conn, room, key string, client crdt.ClientID) {
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
	_, err := readTag(t, c, msgAwareness)
	require.NoError(t, err)
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
	sendWrite(t, live, room, "live", 12)
	assert.ElementsMatch(t, []string{"live"}, srv.GetDoc(room).GetMap("writes").Keys(),
		"only the live connection's write reaches the room")
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
