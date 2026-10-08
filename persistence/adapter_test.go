package persistence_test

import (
	"context"
	"net/http/httptest"
	"strconv"
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

// End to end at the server's default settings, write coalescing included, in a
// room whose state is already stored: the updates reach the store and survive
// a reload, although decoded on its own each batch the server writes parks more
// entries than crdt's default pending cap (100,000), all waiting for the stored
// map. A one-key edit after them must reach the store too: a refused batch
// stays queued, and every later edit is merged into it.
func TestLegacyAdapter_PluggedIntoServer_StoresLargeIncrementalUpdates(t *testing.T) {
	cases := []struct {
		name  string
		sizes []int // keys set by each update the peer sends before the one-key edit
	}{
		{"one update over the cap", []int{100_001}},
		{"two updates under the cap, coalesced into one batch over it", []int{50_001, 50_001}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, updates := nestedMapUpdates(t, append(tc.sizes, 1)...)
			large, edit := updates[:len(tc.sizes)], updates[len(tc.sizes)]
			entries := 0
			for _, n := range tc.sizes {
				entries += n
			}

			ctx := context.Background()
			store := persistence.NewMemoryPersistence()
			_, err := store.AppendUpdate(ctx, "room", base)
			require.NoError(t, err)
			srv := ygws.NewServerWithPersistence(persistence.NewLegacyAdapter(store))
			ts := httptest.NewServer(srv)
			defer ts.Close()
			// Called from require.Eventually's goroutine, so it reports, never fails.
			versions := func() int {
				metas, err := store.ListVersions(ctx, "room")
				if err != nil {
					return -1
				}
				return len(metas)
			}

			connA := dialWS(t, ts, "room")
			drainWS(t, connA, crdt.New())
			for _, u := range large {
				sendV1Update(t, connA, u)
			}
			require.Eventually(t, func() bool { return versions() >= 2 }, 15*time.Second, 20*time.Millisecond,
				"the updates never reached the store")
			v, got := storedNestedEntries(t, store)
			require.Equal(t, persistence.Version(2), v, "the updates were written in more than one batch")
			require.Equal(t, entries, got, "the first batch does not hold every update")

			// CloseRoom writes the edit as the next batch once the room has it.
			sendV1Update(t, connA, edit)
			require.Eventually(t, func() bool { return nestedEntries(srv.GetDoc("room")) == entries+1 },
				15*time.Second, 20*time.Millisecond, "the room never applied the edit")
			require.NoError(t, srv.CloseRoom("room", true))
			require.Equal(t, 3, versions(), "the edit after them never reached the store")

			docB := crdt.New()
			connB := dialWS(t, ts, "room")
			drainWS(t, connB, docB)
			assert.Equal(t, entries+1, nestedEntries(docB), "the reloaded room lost entries")
		})
	}
}

// nestedMapUpdates returns base, in which client 1 creates the map "nested"
// under root map "m", and one incremental update per size, in which client 2
// sets that many more keys on it. Each update is encoded against the state
// before it, so decoded on its own every entry of it parks, waiting for the
// map and, after the first update, for client 2's earlier clocks.
func nestedMapUpdates(t *testing.T, sizes ...int) (base []byte, updates [][]byte) {
	t.Helper()
	author := crdt.New(crdt.WithClientID(1))
	root := author.GetMap("m")
	author.Transact(func(txn *crdt.Transaction) { root.Set(txn, "nested", crdt.NewMapPrelim()) })
	base = crdt.EncodeStateAsUpdateV1(author, nil)

	editor := crdt.New(crdt.WithClientID(2))
	require.NoError(t, crdt.ApplyUpdateV1(editor, base, nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested, ok := v.(*crdt.YMap)
	require.True(t, ok, "nested is %T", v)
	next := 0
	for _, n := range sizes {
		sv := editor.StateVector()
		editor.Transact(func(txn *crdt.Transaction) {
			for range n {
				nested.Set(txn, "k"+strconv.Itoa(next), next)
				next++
			}
		})
		updates = append(updates, crdt.EncodeStateAsUpdateV1(editor, sv))
	}
	return base, updates
}

// nestedEntries returns how many keys the map "nested" under root map "m"
// holds in doc, or -1 when doc has no such map.
func nestedEntries(doc *crdt.Doc) int {
	v, ok := doc.GetMap("m").Get("nested")
	if !ok {
		return -1
	}
	nested, ok := v.(*crdt.YMap)
	if !ok {
		return -1
	}
	return len(nested.Keys())
}

// storedNestedEntries loads "room" from p and returns the version it loaded
// and how many keys the map "nested" under root map "m" holds there.
func storedNestedEntries(t *testing.T, p persistence.VersionedPersistence) (persistence.Version, int) {
	t.Helper()
	lr, err := p.Load(context.Background(), "room")
	require.NoError(t, err)
	doc := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(doc, lr.Update, nil))
	return lr.Version, nestedEntries(doc)
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
	metas, err := store.ListVersions(context.Background(), "room")
	require.NoError(t, err)
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
