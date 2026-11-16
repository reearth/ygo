package websocket

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// A room whose stored state holds a struct of length zero, which ygo 1.55.0
// and earlier could store and the apply now refuses, loads from the bundled
// adapters, since they merge a room's records on load and the merge drops
// that struct.
func TestUnit_LoadRoom_StoredZeroLengthStructIsDroppedOnLoad(t *testing.T) {
	// "abc", and client 3's empty string parked past a gap.
	stored, err := hex.DecodeString("020101000401017403616263010306040101740000")
	if err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdateV1(crdt.New(), stored, nil); !errors.Is(err, crdt.ErrInvalidUpdate) {
		t.Fatalf("the stored state applied with err %v, want ErrInvalidUpdate", err)
	}

	// The file a FilePersistence of ygo 1.55.0 wrote for the room's first
	// update, at <root>/<roomHex>/updates/1.bin.
	dir := t.TempDir()
	updates := filepath.Join(dir, hex.EncodeToString([]byte("room")), "updates")
	if err := os.MkdirAll(updates, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(updates, "1.bin"), stored, 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := persistence.NewFilePersistence(dir)
	if err != nil {
		t.Fatal(err)
	}

	s := NewServerWithPersistence(persistence.NewLegacyAdapter(files))
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	r, _, err := s.getOrCreateRoom(context.Background(), "room")
	if err != nil {
		t.Fatalf("the room did not load: %v", err)
	}
	defer s.releaseInflight(r)
	if got := r.doc.GetText("t").ToString(); got != "abc" {
		t.Fatalf("the room's text is %q, want %q", got, "abc")
	}
	if got := r.doc.PendingStats().Items; got != 0 {
		t.Fatalf("the room parks %d items, want none", got)
	}
}
