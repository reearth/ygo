package websocket_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// TestUnit_Apply_KeystrokeAfterManyDeletionsIsNotTooLarge loads a room whose
// text holds 2,000 separate deletions and types one character into it under
// a 1 KiB MaxUpdateBytes: the update describes the keystroke only.
func TestUnit_Apply_KeystrokeAfterManyDeletionsIsNotTooLarge(t *testing.T) {
	const deletions = 2000
	src := crdt.New()
	txt := src.GetText("t")
	src.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, strings.Repeat("ab", deletions), nil) })
	for i := deletions - 1; i >= 0; i-- {
		src.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 2*i+1, 1) })
	}
	p := ygws.NewMemoryPersistence()
	require.NoError(t, p.StoreUpdate("room", crdt.EncodeStateAsUpdateV1(src, nil)))
	srv := ygws.NewServerWithPersistence(p)
	srv.MaxUpdateBytes = 1024

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		text := doc.GetText("t")
		transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
	})
	require.NoError(t, err)
	require.Equal(t, "x"+strings.Repeat("a", deletions), srv.GetDoc("room").GetText("t").ToString())
}

// TestUnit_Apply_StoredUpdatesReloadEveryDeletion persists a room edited one
// transaction at a time, compacting as it goes, and requires the stored
// updates to rebuild every deletion: each update carries only its own.
func TestUnit_Apply_StoredUpdatesReloadEveryDeletion(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	p.CompactEvery = 3
	srv := ygws.NewServerWithPersistence(p)
	srv.PersistCoalesceWindow = -1
	edit := func(fn func(txn *crdt.Transaction, txt *crdt.YText, m *crdt.YMap)) {
		t.Helper()
		require.NoError(t, srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt, m := doc.GetText("t"), doc.GetMap("m")
			transact(func(txn *crdt.Transaction) { fn(txn, txt, m) })
		}))
	}
	edit(func(txn *crdt.Transaction, txt *crdt.YText, _ *crdt.YMap) { txt.Insert(txn, 0, "abcdefgh", nil) })
	edit(func(txn *crdt.Transaction, txt *crdt.YText, _ *crdt.YMap) { txt.Delete(txn, 1, 1) })
	edit(func(txn *crdt.Transaction, _ *crdt.YText, m *crdt.YMap) { m.Set(txn, "k", "v1") })
	edit(func(txn *crdt.Transaction, txt *crdt.YText, _ *crdt.YMap) { txt.Delete(txn, 4, 2) })
	edit(func(txn *crdt.Transaction, _ *crdt.YText, m *crdt.YMap) { m.Set(txn, "k", "v2") })
	edit(func(txn *crdt.Transaction, txt *crdt.YText, _ *crdt.YMap) {
		txt.Delete(txn, 0, 1)
		txt.Insert(txn, 0, "z", nil)
	})
	require.NoError(t, srv.Shutdown(context.Background()))

	stored, err := p.LoadDoc("room")
	require.NoError(t, err)
	reloaded := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(reloaded, stored, nil))
	require.Equal(t, "zcdeh", reloaded.GetText("t").ToString())
	v, ok := reloaded.GetMap("m").Get("k")
	require.True(t, ok)
	require.Equal(t, "v2", v)
}
