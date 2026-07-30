package persistence_test

import (
	"testing"

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
