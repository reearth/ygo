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
