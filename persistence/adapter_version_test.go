package persistence_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// seedRoom appends n edits to room and returns the expected text length.
func seedRoom(t *testing.T, store persistence.VersionedPersistence, room string, n int) {
	t.Helper()
	ctx := context.Background()
	doc := crdt.New()
	txt := doc.GetText("t")
	for i := 0; i < n; i++ {
		before := crdt.EncodeStateVectorV1(doc)
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
		sv, err := crdt.DecodeStateVectorV1(before)
		require.NoError(t, err)
		diff := crdt.EncodeStateAsUpdateV1(doc, sv)
		_, err = store.AppendUpdate(ctx, room, diff)
		require.NoError(t, err)
	}
}

// SaveVersion captures the room's current head as an enumerable labelled
// snapshot whose state materializes back to that head.
func TestLegacyAdapterSaveVersion_CapturesHead(t *testing.T) {
	ctx := context.Background()
	store := persistence.NewMemoryPersistence()
	a := persistence.NewLegacyAdapter(store)

	seedRoom(t, store, "room", 3)

	id, err := a.SaveVersion(ctx, "room", "before-migration")
	require.NoError(t, err)
	assert.NotZero(t, id, "a non-empty room yields a real snapshot id")

	snaps, err := store.ListSnapshots(ctx, "room")
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, "before-migration", snaps[0].Label)
	assert.Equal(t, id, snaps[0].ID)

	// The stored state must reconstruct the same document.
	state, err := store.GetSnapshotState(ctx, "room", id)
	require.NoError(t, err)
	got := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(got, state, nil))
	assert.Equal(t, 3, got.GetText("t").Len())
}

// An empty room has no state worth versioning: no snapshot, no error.
func TestLegacyAdapterSaveVersion_EmptyRoomIsNoOp(t *testing.T) {
	ctx := context.Background()
	store := persistence.NewMemoryPersistence()
	a := persistence.NewLegacyAdapter(store)

	id, err := a.SaveVersion(ctx, "empty", "lbl")
	require.NoError(t, err)
	assert.Zero(t, id, "empty room yields no snapshot")

	snaps, err := store.ListSnapshots(ctx, "empty")
	require.NoError(t, err)
	assert.Empty(t, snaps)
}

// KeepSnapshots bounds retention: the oldest auto versions are trimmed so the
// history cannot grow without limit.
func TestLegacyAdapterSaveVersion_KeepSnapshotsRetention(t *testing.T) {
	ctx := context.Background()
	store := persistence.NewMemoryPersistence()
	a := persistence.NewLegacyAdapter(store)
	a.KeepSnapshots = 2

	seedRoom(t, store, "room", 1)

	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := a.SaveVersion(ctx, "room", "auto")
		require.NoError(t, err)
		ids = append(ids, id)
	}

	snaps, err := store.ListSnapshots(ctx, "room")
	require.NoError(t, err)
	require.Len(t, snaps, 2, "retention should keep only the newest KeepSnapshots")
	// Newest-first: the two most recent ids survive.
	assert.Equal(t, ids[4], snaps[0].ID)
	assert.Equal(t, ids[3], snaps[1].ID)
}

// KeepSnapshots == 0 (default) keeps everything, matching KeepVersions.
func TestLegacyAdapterSaveVersion_ZeroKeepsAll(t *testing.T) {
	ctx := context.Background()
	store := persistence.NewMemoryPersistence()
	a := persistence.NewLegacyAdapter(store)

	seedRoom(t, store, "room", 1)
	for i := 0; i < 4; i++ {
		_, err := a.SaveVersion(ctx, "room", "auto")
		require.NoError(t, err)
	}

	snaps, err := store.ListSnapshots(ctx, "room")
	require.NoError(t, err)
	assert.Len(t, snaps, 4, "KeepSnapshots=0 must retain every version")
}

// bareStore is a VersionedPersistence that does NOT implement SnapshotStore, so
// SaveVersion has nowhere to put a version.
type bareStore struct {
	persistence.VersionedPersistence
}

// A store without snapshot support must report the misconfiguration rather than
// silently discarding versions.
func TestLegacyAdapterSaveVersion_UnsupportedStoreErrors(t *testing.T) {
	ctx := context.Background()
	inner := persistence.NewMemoryPersistence()
	a := persistence.NewLegacyAdapter(bareStore{VersionedPersistence: inner})

	seedRoom(t, inner, "room", 1)

	_, err := a.SaveVersion(ctx, "room", "lbl")
	assert.ErrorIs(t, err, persistence.ErrSnapshotsUnsupported,
		"a store without SnapshotStore must report the misconfiguration")
}

// An auto-captured version must never evict a snapshot a user deliberately
// named: the two classes have opposite retention needs, and the named one is
// exactly what a user would be most upset to lose (issue #212).
func TestLegacyAdapterSaveVersion_AutoDoesNotEvictNamed(t *testing.T) {
	ctx := context.Background()
	store := persistence.NewMemoryPersistence()
	a := persistence.NewLegacyAdapter(store)
	a.KeepSnapshots = 2

	seedRoom(t, store, "room", 1)

	named, err := a.SaveVersion(ctx, "room", "before-migration")
	require.NoError(t, err)

	// Enough auto versions to bury the named one under the old newest-N rule.
	for i := 0; i < 5; i++ {
		_, err := a.SaveVersion(ctx, "room", "auto")
		require.NoError(t, err)
	}

	snaps, err := store.ListSnapshots(ctx, "room")
	require.NoError(t, err)

	var labels []string
	found := false
	autos := 0
	for _, sn := range snaps {
		labels = append(labels, sn.Label)
		if sn.ID == named {
			found = true
		}
		if sn.Label == "auto" {
			autos++
		}
	}
	assert.True(t, found, "named snapshot must survive auto-version retention; labels = %v", labels)
	assert.Equal(t, 2, autos, "the auto class must still be bounded by KeepSnapshots")
}

// KeepSnapshots bounds each label class independently.
func TestLegacyAdapterTrimSnapshots_PerLabelBound(t *testing.T) {
	ctx := context.Background()
	store := persistence.NewMemoryPersistence()
	a := persistence.NewLegacyAdapter(store)
	a.KeepSnapshots = 2

	seedRoom(t, store, "room", 1)
	for i := 0; i < 4; i++ {
		_, err := a.SaveVersion(ctx, "room", "auto")
		require.NoError(t, err)
	}
	for i := 0; i < 4; i++ {
		_, err := a.SaveVersion(ctx, "room", "manual")
		require.NoError(t, err)
	}

	snaps, err := store.ListSnapshots(ctx, "room")
	require.NoError(t, err)
	counts := map[string]int{}
	for _, sn := range snaps {
		counts[sn.Label]++
	}
	assert.Equal(t, 2, counts["auto"], "auto class bounded")
	assert.Equal(t, 2, counts["manual"], "manual class bounded independently")
	assert.Len(t, snaps, 4, "room total is the sum of the bounded classes")
}
