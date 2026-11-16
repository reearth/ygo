package websocket_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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
