package websocket_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// makeUpdate builds a V1 update that inserts text into YText "t".
func makeUpdate(t *testing.T, text string) []byte {
	t.Helper()
	d := crdt.New()
	txt := d.GetText("t")
	d.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, text, nil) })
	return crdt.EncodeStateAsUpdateV1(d, nil)
}

// makeAwareness builds a one-client awareness update for the given client ID.
func makeAwareness(id uint64) []byte {
	a := awareness.New(id)
	a.SetLocalState(map[string]any{"cursor": 1})
	return a.EncodeUpdate([]uint64{id})
}

// dialReadOnly dials the test server, marking the connection read-only via a
// header the test's Authorize func keys off of.
func dialReadOnly(t *testing.T, ts *httptest.Server, room string, readOnly bool) *gws.Conn {
	t.Helper()
	h := http.Header{}
	if readOnly {
		h.Set("X-Read-Only", "1")
	}
	conn, _, err := gws.DefaultDialer.Dial(wsURL(ts, room), h)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readOnlyByHeader authorizes every connection but marks it read-only when the
// X-Read-Only header is set.
func readOnlyByHeader(r *http.Request) (ygws.ConnectionConfig, bool) {
	return ygws.ConnectionConfig{ReadOnly: r.Header.Get("X-Read-Only") == "1"}, true
}

// #59 — a read-only peer's inbound document update must NOT mutate server state.
func TestReadOnly_InboundUpdateDropped(t *testing.T) {
	srv := ygws.NewServer()
	srv.Authorize = func(*http.Request) (ygws.ConnectionConfig, bool) {
		return ygws.ConnectionConfig{ReadOnly: true}, true
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "room")
	drainHandshake(t, conn, crdt.New())

	sendUpdate(t, conn, makeUpdate(t, "hello"))
	time.Sleep(100 * time.Millisecond)

	doc := srv.GetDoc("room")
	require.NotNil(t, doc)
	assert.Empty(t, doc.GetText("t").ToString(),
		"read-only peer's update must not mutate server state")
}

// #59 — a read-only peer still receives broadcasts from read-write peers.
func TestReadOnly_ReceivesBroadcasts(t *testing.T) {
	srv := ygws.NewServer()
	srv.Authorize = readOnlyByHeader
	ts := httptest.NewServer(srv)
	defer ts.Close()

	editor := dialReadOnly(t, ts, "room", false)
	drainHandshake(t, editor, crdt.New())
	reader := dialReadOnly(t, ts, "room", true)
	docReader := crdt.New()
	drainHandshake(t, reader, docReader)

	sendUpdate(t, editor, makeUpdate(t, "hello"))

	// The read-only peer should receive the broadcast (skip any awareness frames).
	deadline := time.Now().Add(3 * time.Second)
	for docReader.GetText("t").ToString() != "hello" {
		if time.Now().After(deadline) {
			t.Fatalf("read-only peer never received the broadcast; text = %q",
				docReader.GetText("t").ToString())
		}
		if outerType, payload := readOne(t, reader, 2*time.Second); outerType == 0 {
			_, _ = ygsync.ApplySyncMessage(docReader, payload, nil)
		}
	}
}

// #59 — a read-only peer still receives a SyncStep2 in reply to its SyncStep1
// (read-only drops writes, not reads).
func TestReadOnly_SyncStep1StillAnswered(t *testing.T) {
	srv := ygws.NewServer()
	srv.Authorize = func(*http.Request) (ygws.ConnectionConfig, bool) {
		return ygws.ConnectionConfig{ReadOnly: true}, true
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "room")
	drainHandshake(t, conn, crdt.New())

	sendSync(t, conn, ygsync.EncodeSyncStep1(crdt.New()))

	outerType, payload := readOne(t, conn, 2*time.Second)
	require.Equal(t, uint64(0), outerType, "expected a sync reply")
	syncType, err := encoding.NewDecoder(payload).ReadVarUint()
	require.NoError(t, err)
	assert.Equal(t, uint64(ygsync.MsgSyncStep2), syncType,
		"read-only peer's SyncStep1 must be answered with SyncStep2")
}

// #59 — a read-only peer's inbound awareness must NOT be recorded server-side.
func TestReadOnly_InboundAwarenessDropped(t *testing.T) {
	srv := ygws.NewServer()
	srv.Authorize = func(*http.Request) (ygws.ConnectionConfig, bool) {
		return ygws.ConnectionConfig{ReadOnly: true}, true
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "room")
	drainHandshake(t, conn, crdt.New())

	const roClient = uint64(7777)
	sendAwareness(t, conn, makeAwareness(roClient))
	time.Sleep(100 * time.Millisecond)

	aw, ok := srv.GetAwareness("room")
	require.True(t, ok)
	_, present := aw.GetStates()[roClient]
	assert.False(t, present, "read-only peer's awareness must not be recorded server-side")
}

// #59 — the existing bool AuthFunc keeps working: false rejects, true accepts
// as read-write.
func TestReadOnly_LegacyAuthFuncStillWorks(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		srv := ygws.NewServer()
		srv.AuthFunc = func(*http.Request) bool { return false }
		ts := httptest.NewServer(srv)
		defer ts.Close()
		_, _, err := gws.DefaultDialer.Dial(wsURL(ts, "room"), nil)
		assert.Error(t, err, "AuthFunc=false must reject the upgrade")
	})
	t.Run("accept_read_write", func(t *testing.T) {
		srv := ygws.NewServer()
		srv.AuthFunc = func(*http.Request) bool { return true }
		ts := httptest.NewServer(srv)
		defer ts.Close()
		conn := dial(t, ts, "room")
		drainHandshake(t, conn, crdt.New())
		sendUpdate(t, conn, makeUpdate(t, "hello"))
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, "hello", srv.GetDoc("room").GetText("t").ToString(),
			"legacy AuthFunc-accepted peer must be read-write")
	})
}

// #59 — when both AuthFunc and Authorize are set, the richer Authorize wins.
func TestReadOnly_AuthorizeTakesPrecedenceOverAuthFunc(t *testing.T) {
	srv := ygws.NewServer()
	srv.AuthFunc = func(*http.Request) bool { return true } // would allow read-write
	srv.Authorize = func(*http.Request) (ygws.ConnectionConfig, bool) {
		return ygws.ConnectionConfig{ReadOnly: true}, true // richer one wins → read-only
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "room")
	drainHandshake(t, conn, crdt.New())
	sendUpdate(t, conn, makeUpdate(t, "hello"))
	time.Sleep(100 * time.Millisecond)

	assert.Empty(t, srv.GetDoc("room").GetText("t").ToString(),
		"Authorize must take precedence over AuthFunc")
}
