package persistence_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// The shim must satisfy the provider's PersistenceAdapter interface.
var _ ygws.PersistenceAdapter = (*persistence.LegacyAdapter)(nil)

func TestLegacyAdapter_LoadDoc_StoreUpdate(t *testing.T) {
	store := persistence.NewMemoryPersistence()
	ad := persistence.NewLegacyAdapter(store)

	// Empty room → LoadDoc returns (nil, nil) per PersistenceAdapter contract.
	got, err := ad.LoadDoc("room")
	require.NoError(t, err)
	assert.Nil(t, got)

	// StoreUpdate appends; LoadDoc returns the merged head.
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })
	upd := crdt.EncodeStateAsUpdateV1(doc, nil)
	require.NoError(t, ad.StoreUpdate("room", upd))

	got, err = ad.LoadDoc("room")
	require.NoError(t, err)
	require.NotNil(t, got)

	d2 := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(d2, got, nil))
	assert.Equal(t, "hi", d2.GetText("t").ToString())
}

// End-to-end: a VersionedPersistence plugged into the WS server via the shim
// persists peer edits and reloads them for a later peer.
func TestLegacyAdapter_PluggedIntoServer(t *testing.T) {
	store := persistence.NewMemoryPersistence()
	srv := ygws.NewServerWithPersistence(persistence.NewLegacyAdapter(store))
	// Disable persistence coalescing (default-on as of v1.36.0, #175) so the
	// seeded edit is persisted immediately rather than after the debounce
	// window — this test exercises the shim + reload path, not write timing.
	srv.PersistCoalesceWindow = -1
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Peer A seeds the room.
	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "persisted", nil) })
	sendV1Update(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))
	time.Sleep(100 * time.Millisecond)

	// The store should now hold at least one version for the room.
	metas, err := store.ListVersions(context.Background(), "room")
	require.NoError(t, err)
	assert.NotEmpty(t, metas)

	// Close the room so it must reload from persistence on next connect.
	require.NoError(t, srv.CloseRoom("room", true))

	// Peer B connects fresh; handshake step-2 must contain the persisted text.
	docB := crdt.New(crdt.WithClientID(2))
	connB := dialWS(t, ts, "room")
	drainWS(t, connB, docB)
	assert.Equal(t, "persisted", docB.GetText("t").ToString())
}

func TestLegacyAdapter_CompactForwardsWithKeep(t *testing.T) {
	store := persistence.NewMemoryPersistence()
	ad := persistence.NewLegacyAdapter(store)
	ad.KeepVersions = 2

	// Append 5 versions.
	for i := 0; i < 5; i++ {
		doc := crdt.New(crdt.WithClientID(crdt.ClientID(i + 1)))
		txt := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
		require.NoError(t, ad.StoreUpdate("room", crdt.EncodeStateAsUpdateV1(doc, nil)))
	}

	// Compact via the shim → must forward to store.Compact(room, 2).
	require.NoError(t, ad.Compact(context.Background(), "room"))

	metas, err := store.ListVersions(context.Background(), "room")
	require.NoError(t, err)
	assert.LessOrEqual(t, len(metas), 2, "history should be trimmed to KeepVersions")

	// State is preserved (materialised head still loads).
	head, err := ad.LoadDoc("room")
	require.NoError(t, err)
	assert.NotEmpty(t, head)
}

func TestLegacyAdapter_CompactKeepZeroIsNoop(t *testing.T) {
	store := persistence.NewMemoryPersistence()
	ad := persistence.NewLegacyAdapter(store) // KeepVersions defaults to 0
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
	require.NoError(t, ad.StoreUpdate("room", crdt.EncodeStateAsUpdateV1(doc, nil)))
	require.NoError(t, ad.Compact(context.Background(), "room")) // keep=0 → keep all, no error
	metas, _ := store.ListVersions(context.Background(), "room")
	assert.Len(t, metas, 1)
}

// ── minimal WS test helpers (self-contained; persistence_test package) ──

func dialWS(t *testing.T, ts *httptest.Server, room string) *gws.Conn {
	t.Helper()
	url := "ws" + ts.URL[len("http"):] + "/" + room
	conn, _, err := gws.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func drainWS(t *testing.T, conn *gws.Conn, doc *crdt.Doc) {
	t.Helper()
	for i := 0; i < 3; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := conn.ReadMessage()
		_ = conn.SetReadDeadline(time.Time{})
		require.NoError(t, err)
		dec := encoding.NewDecoder(data)
		outer, err := dec.ReadVarUint()
		require.NoError(t, err)
		if outer == 0 { // msgSync
			_, _ = ygsync.ApplySyncMessage(doc, dec.RemainingBytes(), nil)
		}
	}
}

func sendV1Update(t *testing.T, conn *gws.Conn, update []byte) {
	t.Helper()
	inner := encoding.NewEncoder()
	inner.WriteVarUint(ygsync.MsgUpdate)
	inner.WriteVarBytes(update)
	outer := encoding.NewEncoder()
	outer.WriteVarUint(0) // msgSync
	outer.WriteRaw(inner.Bytes())
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, outer.Bytes()))
}
