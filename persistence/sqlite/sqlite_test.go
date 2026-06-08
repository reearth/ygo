package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/reearth/ygo/persistence/sqlite"
)

func TestOpen_CreatesSchemaAndCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	lr, err := s.Load(context.Background(), "room")
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if lr.Update != nil || lr.Version != 0 {
		t.Fatalf("empty room: got Update=%v Version=%d, want nil/0", lr.Update, lr.Version)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
