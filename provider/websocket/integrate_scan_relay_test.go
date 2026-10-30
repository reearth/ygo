package websocket_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// storeRecorder is a PersistenceAdapter that loads state and records every
// update the server stores.
type storeRecorder struct {
	state  []byte
	mu     sync.Mutex
	stored [][]byte
}

func (r *storeRecorder) LoadDoc(string) ([]byte, error) { return r.state, nil }

func (r *storeRecorder) StoreUpdate(_ string, update []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stored = append(r.stored, update)
	return nil
}

func (r *storeRecorder) storedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stored)
}

// A room whose document has a scan bound (set in OnLoadDocument) refuses an
// update whose conflict scans pass the bound, and rolls it back whole: the
// update and the deletion it carries reach no other peer, the sender is
// answered SyncStatus(0), nothing is stored, and the room is as it was.
func TestInteg_IntegrateScan_RefusedUpdateReachesNoPeerAndStoresNothing(t *testing.T) {
	const l, bound = 200, 50
	// The room's text is l characters typed one at a time at the end by two
	// clients in turn, so no two neighbours merge and each one's left origin
	// is the character before it.
	a, b := crdt.New(crdt.WithClientID(5)), crdt.New(crdt.WithClientID(6))
	var first []byte
	for i := range l {
		w, r := a, b
		if i%2 == 1 {
			w, r = b, a
		}
		txt := w.GetText("t")
		before := r.StateVector()
		w.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, i, "x", nil) })
		require.NoError(t, crdt.ApplyUpdateV1(r, crdt.EncodeStateAsUpdateV1(w, before), nil))
		if i == 0 {
			first = crdt.EncodeStateAsUpdateV1(a, nil)
		}
	}
	rec := &storeRecorder{state: crdt.EncodeStateAsUpdateV1(a, nil)}

	srv := ygws.NewServerWithPersistence(rec)
	srv.HocuspocusFraming = true
	srv.PersistCoalesceWindow = -1
	srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
		doc.SetMaxIntegrateScan(bound)
		return nil
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	watcherDoc := crdt.New(crdt.WithClientID(9))
	watcher := hpDial(t, ts, "room", false, watcherDoc)
	watcherSyncBytes(t, watcher, watcherDoc)
	require.Equal(t, strings.Repeat("x", l), watcherDoc.GetText("t").ToString())
	room := srv.GetDoc("room")
	require.NotNil(t, room)
	before := crdt.EncodeStateAsUpdateV1(room, nil)

	// The editor saw only the first character. It deletes it and inserts
	// after it, so its insert names the first character as its left origin
	// and scans the l-1 characters after it in the room, past the bound.
	editor := crdt.New(crdt.WithClientID(7))
	require.NoError(t, crdt.ApplyUpdateV1(editor, first, nil))
	sv := editor.StateVector()
	etxt := editor.GetText("t")
	editor.Transact(func(txn *crdt.Transaction) {
		etxt.Insert(txn, 1, "y", nil)
		etxt.Delete(txn, 0, 1)
	})
	update := crdt.EncodeStateAsUpdateV1(editor, sv)
	{
		probe := crdt.New()
		require.NoError(t, crdt.ApplyUpdateV1(probe, rec.state, nil))
		probe.SetMaxIntegrateScan(bound)
		require.ErrorIs(t, crdt.ApplyUpdateV1(probe, update, nil), crdt.ErrIntegrateScanBudget,
			"the update must pass the bound for this test to mean anything")
	}

	conn := hpDial(t, ts, "room", false, crdt.New())
	storedBefore := rec.storedCount()
	hpSend(t, conn, "room", tagSync, ygsync.EncodeUpdate(update))
	assert.Equal(t, []uint64{0}, syncStatuses(t, conn, "room"), "the refused update is answered SyncStatus(0)")

	assert.Zero(t, watcherSyncBytes(t, watcher, watcherDoc), "the refused update reached another peer")
	assert.Equal(t, strings.Repeat("x", l), watcherDoc.GetText("t").ToString())
	assert.Equal(t, storedBefore, rec.storedCount(), "the refused update was stored")
	assert.Equal(t, before, crdt.EncodeStateAsUpdateV1(room, nil), "the room's document changed")
}
