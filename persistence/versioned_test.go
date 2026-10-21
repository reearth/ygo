package persistence_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/persistence"
)

func TestConformance_Memory(t *testing.T) {
	persistence.RunConformance(t, func() persistence.VersionedPersistence {
		return persistence.NewMemoryPersistence()
	})
}

func TestConformance_File(t *testing.T) {
	persistence.RunConformance(t, func() persistence.VersionedPersistence {
		dir := t.TempDir()
		p, err := persistence.NewFilePersistence(dir)
		if err != nil {
			t.Fatalf("NewFilePersistence: %v", err)
		}
		return p
	})
}

func TestSnapshotStoreConformance_Memory(t *testing.T) {
	persistence.RunSnapshotStoreConformance(t, func() persistence.SnapshotStore {
		return persistence.NewMemoryPersistence()
	})
}

func TestSnapshotStoreConformance_File(t *testing.T) {
	persistence.RunSnapshotStoreConformance(t, func() persistence.SnapshotStore {
		dir := t.TempDir()
		p, err := persistence.NewFilePersistence(dir)
		if err != nil {
			t.Fatalf("NewFilePersistence: %v", err)
		}
		return p
	})
}

func TestRoomListerConformance_Memory(t *testing.T) {
	persistence.RunRoomListerConformance(t, func() persistence.SnapshotVersionedPersistence {
		return persistence.NewMemoryPersistence()
	})
}

func TestRoomListerConformance_File(t *testing.T) {
	persistence.RunRoomListerConformance(t, func() persistence.SnapshotVersionedPersistence {
		dir := t.TempDir()
		p, err := persistence.NewFilePersistence(dir)
		if err != nil {
			t.Fatalf("NewFilePersistence: %v", err)
		}
		return p
	})
}

// Decoded without the state it was made against, an incremental update parks
// every item that depends on that state. The bundled stores check an update by
// decoding it alone, so the check must keep such an update however many items
// it parks there: the room has already applied it, and refusing it loses the
// edit. An update that does not decode is still refused and takes no version.
// The sqlite store runs the same case in its own package.
func TestAppendUpdate_KeepsLargeIncrementalUpdate(t *testing.T) {
	stores := []struct {
		name string
		open func(t *testing.T) persistence.VersionedPersistence
	}{
		{"MemoryPersistence", func(*testing.T) persistence.VersionedPersistence {
			return persistence.NewMemoryPersistence()
		}},
		{"FilePersistence", func(t *testing.T) persistence.VersionedPersistence {
			p, err := persistence.NewFilePersistence(t.TempDir())
			require.NoError(t, err)
			return p
		}},
	}
	const entries = 100_001 // one more than crdt's default pending cap
	base, updates := nestedMapUpdates(t, entries)
	for _, s := range stores {
		t.Run(s.name, func(t *testing.T) {
			p := s.open(t)
			ctx := context.Background()
			for _, u := range [][]byte{base, updates[0]} {
				_, err := p.AppendUpdate(ctx, "room", u)
				require.NoError(t, err)
			}
			_, err := p.AppendUpdate(ctx, "room", []byte{0xff, 0xff, 0xff})
			require.Error(t, err, "AppendUpdate accepted an update that does not decode")
			v, got := storedNestedEntries(t, p)
			assert.Equal(t, persistence.Version(2), v)
			assert.Equal(t, entries, got)
		})
	}
}
