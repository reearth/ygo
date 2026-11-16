package websocket_test

import (
	"errors"
	"fmt"
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
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// Tests for ConnectionConfig.Identity, which reaches Server.OnPeerUpdate as
// PeerUpdateInfo.Identity.

type testUser struct{ ID string }

// identityRecorder is an OnPeerUpdate hook that applies every update and
// records the Identity each one arrived with, in arrival order.
type identityRecorder struct {
	mu   sync.Mutex
	seen []any
}

func (r *identityRecorder) hook(info ygws.PeerUpdateInfo, apply func() error) error {
	r.mu.Lock()
	r.seen = append(r.seen, info.Identity)
	r.mu.Unlock()
	return apply()
}

func (r *identityRecorder) identities() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.seen...)
}

// hpDialAs dials with an X-User header the test's Authorize keys off of.
func hpDialAs(t *testing.T, ts *httptest.Server, room, user string, doc *crdt.Doc) *gws.Conn {
	t.Helper()
	h := http.Header{}
	if user != "" {
		h.Set("X-User", user)
	}
	conn, _, err := gws.DefaultDialer.Dial(wsURL(ts, room), h)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	hpDrainHandshake(t, conn, room, doc)
	return conn
}

// userByHeader authorizes every connection, attaching the X-User header as
// the identity when it is set.
func userByHeader(r *http.Request) (ygws.ConnectionConfig, bool) {
	var cfg ygws.ConnectionConfig
	if id := r.Header.Get("X-User"); id != "" {
		cfg.Identity = testUser{ID: id}
	}
	return cfg, true
}

// sendTokenAuth sends a Hocuspocus Auth (tag 2) Token message.
func sendTokenAuth(t *testing.T, conn *gws.Conn, room, token string) {
	t.Helper()
	hpSend(t, conn, room, 2, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(0) // Token
		enc.WriteVarString(token)
	}))
}

// Each update reaches the hook with the Identity Authorize attached to the
// connection that sent it, so two users in one room are told apart.
func TestInteg_OnPeerUpdate_IdentityFromAuthorize(t *testing.T) {
	t.Parallel()
	var rec identityRecorder
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.Authorize = userByHeader
		srv.OnPeerUpdate = rec.hook
	})
	aliceDoc := crdt.New(crdt.WithClientID(7))
	bobDoc := crdt.New(crdt.WithClientID(8))
	alice := hpDialAs(t, ts, "room", "alice", aliceDoc)
	bob := hpDialAs(t, ts, "room", "bob", bobDoc)

	hpSend(t, alice, "room", tagSync, ygsync.EncodeUpdate(textUpdate(aliceDoc, "a")))
	require.Equal(t, []uint64{1}, syncStatuses(t, alice, "room"))
	hpSend(t, bob, "room", tagSync, ygsync.EncodeUpdate(textUpdate(bobDoc, "b")))
	require.Equal(t, []uint64{1}, syncStatuses(t, bob, "room"))

	assert.Equal(t, []any{testUser{ID: "alice"}, testUser{ID: "bob"}}, rec.identities())
}

// A connection Authorize attached no identity to, or one accepted without
// Authorize, sends its updates with a nil Identity.
func TestInteg_OnPeerUpdate_IdentityNilWhenNoneAttached(t *testing.T) {
	t.Parallel()
	t.Run("Authorize without an identity", func(t *testing.T) {
		t.Parallel()
		var rec identityRecorder
		_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
			srv.Authorize = userByHeader
			srv.OnPeerUpdate = rec.hook
		})
		doc := crdt.New(crdt.WithClientID(7))
		conn := hpDialAs(t, ts, "room", "", doc)
		hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "x")))
		require.Equal(t, []uint64{1}, syncStatuses(t, conn, "room"))
		assert.Equal(t, []any{nil}, rec.identities())
	})
	t.Run("AuthFunc", func(t *testing.T) {
		t.Parallel()
		var rec identityRecorder
		_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
			srv.AuthFunc = func(*http.Request) bool { return true }
			srv.OnPeerUpdate = rec.hook
		})
		doc := crdt.New(crdt.WithClientID(7))
		conn := hpDialAs(t, ts, "room", "alice", doc)
		hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "x")))
		require.Equal(t, []uint64{1}, syncStatuses(t, conn, "room"))
		assert.Equal(t, []any{nil}, rec.identities())
	})
}

// An accepted in-band token replaces the connection's ConnectionConfig whole:
// updates sent after it carry the Identity OnTokenAuth returned, nil
// included, and updates sent before it keep the one Authorize attached.
func TestInteg_OnPeerUpdate_IdentityFromOnTokenAuth(t *testing.T) {
	t.Parallel()
	t.Run("replaces Authorize's identity", func(t *testing.T) {
		t.Parallel()
		var rec identityRecorder
		_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
			srv.Authorize = userByHeader
			srv.OnTokenAuth = func(_, token string) (ygws.ConnectionConfig, error) {
				return ygws.ConnectionConfig{Identity: testUser{ID: token}}, nil
			}
			srv.OnPeerUpdate = rec.hook
		})
		doc := crdt.New(crdt.WithClientID(7))
		conn := hpDialAs(t, ts, "room", "http-user", doc)

		hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "before")))
		sendTokenAuth(t, conn, "room", "token-user")
		hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "after")))
		require.Equal(t, []uint64{1, 1}, syncStatuses(t, conn, "room"))

		assert.Equal(t, []any{testUser{ID: "http-user"}, testUser{ID: "token-user"}}, rec.identities())
	})
	t.Run("a nil identity clears Authorize's", func(t *testing.T) {
		t.Parallel()
		var rec identityRecorder
		_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
			srv.Authorize = userByHeader
			srv.OnTokenAuth = func(string, string) (ygws.ConnectionConfig, error) {
				return ygws.ConnectionConfig{}, nil
			}
			srv.OnPeerUpdate = rec.hook
		})
		doc := crdt.New(crdt.WithClientID(7))
		conn := hpDialAs(t, ts, "room", "http-user", doc)

		sendTokenAuth(t, conn, "room", "anything")
		hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "x")))
		require.Equal(t, []uint64{1}, syncStatuses(t, conn, "room"))

		assert.Equal(t, []any{nil}, rec.identities())
	})
}

// A denied token leaves no identity behind: the connection closes 4401 and
// nothing it sent after the token reaches the hook, even when the hook
// returned a populated ConnectionConfig alongside its error.
//
// This pins the observable behaviour, not handleAuth's assignment order:
// the close drops all later input, so applying a denied cfg early would not
// change what the hook sees.
func TestInteg_OnPeerUpdate_IdentityNotSetByDeniedToken(t *testing.T) {
	t.Parallel()
	var rec identityRecorder
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.Authorize = userByHeader
		srv.OnTokenAuth = func(string, string) (ygws.ConnectionConfig, error) {
			// A populated config next to an error must not be applied.
			return ygws.ConnectionConfig{Identity: testUser{ID: "admin"}}, errors.New("denied")
		}
		srv.OnPeerUpdate = rec.hook
	})
	doc := crdt.New(crdt.WithClientID(7))
	conn := hpDialAs(t, ts, "room", "http-user", doc)

	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "before")))
	require.Equal(t, []uint64{1}, syncStatuses(t, conn, "room"))

	sendTokenAuth(t, conn, "room", "bad")
	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(textUpdate(doc, "after")))

	// Read to the close so the server has processed both frames.
	var closeErr error
	for closeErr == nil {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, closeErr = conn.ReadMessage()
	}
	assert.True(t, gws.IsCloseError(closeErr, 4401), "want 4401 close, got %v", closeErr)
	assert.Equal(t, []any{testUser{ID: "http-user"}}, rec.identities(),
		"the update after a denied token must not reach the hook, under any identity")
}

// Updates sent concurrently by many connections in one room each reach the
// hook under their own sender's identity, with no cross-talk. This is the
// case that puts several read loops in one room under -race.
func TestInteg_OnPeerUpdate_IdentityConcurrentConnections(t *testing.T) {
	t.Parallel()
	const (
		conns          = 8
		updatesPerConn = 10
	)
	var rec identityRecorder
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.Authorize = userByHeader
		srv.OnPeerUpdate = rec.hook
	})

	// Dial on this goroutine: the helpers assert with require.
	type client struct {
		conn *gws.Conn
		doc  *crdt.Doc
		user string
	}
	clients := make([]client, conns)
	for i := range clients {
		user := fmt.Sprintf("user-%d", i)
		doc := crdt.New(crdt.WithClientID(crdt.ClientID(100 + i)))
		clients[i] = client{conn: hpDialAs(t, ts, "room", user, doc), doc: doc, user: user}
	}

	var wg sync.WaitGroup
	errs := make([]error, conns)
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := clients[i]
			for n := 0; n < updatesPerConn; n++ {
				frame := encoding.EncodeBytes(func(enc *encoding.Encoder) {
					enc.WriteVarString("room")
					enc.WriteVarUint(tagSync)
					enc.WriteRaw(ygsync.EncodeUpdate(textUpdate(c.doc, "x")))
				})
				if err := c.conn.WriteMessage(gws.BinaryMessage, frame); err != nil {
					errs[i] = err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "connection %d", i)
	}

	// Drain each connection's answers so every update has been applied.
	for _, c := range clients {
		require.Len(t, syncStatuses(t, c.conn, "room"), updatesPerConn)
	}

	counts := map[string]int{}
	for _, id := range rec.identities() {
		user, ok := id.(testUser)
		require.True(t, ok, "every update must carry an identity, got %#v", id)
		counts[user.ID]++
	}
	want := map[string]int{}
	for _, c := range clients {
		want[c.user] = updatesPerConn
	}
	assert.Equal(t, want, counts, "each update must carry its own sender's identity")
}
