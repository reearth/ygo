package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
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

// twoUpdates returns two valid incremental V1 updates for ONE document such
// that storing #1 then #1+#2 merges to text "ab". u1 is the full state after
// inserting "a"; u2 is the diff (since the post-"a" state vector) introduced by
// inserting "b" at index 1.
func twoUpdates(t *testing.T) (a, b []byte) {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "a", nil) })
	a = crdt.EncodeStateAsUpdateV1(doc, nil)
	svAfterA := doc.StateVector()
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 1, "b", nil) })
	b = crdt.EncodeStateAsUpdateV1(doc, svAfterA)

	// Sanity-check the construction before relying on it in the store tests.
	merged, err := crdt.MergeUpdatesV1(a, b)
	if err != nil {
		t.Fatalf("twoUpdates: merge: %v", err)
	}
	check := crdt.New()
	if err := crdt.ApplyUpdateV1(check, merged, nil); err != nil {
		t.Fatalf("twoUpdates: apply merged: %v", err)
	}
	if got := check.GetText("t").ToString(); got != "ab" {
		t.Fatalf("twoUpdates: merged text = %q, want %q", got, "ab")
	}
	return a, b
}

func TestAppendLoad_RoundTrip(t *testing.T) {
	s, _ := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	ctx := context.Background()
	a, b := twoUpdates(t)
	v1, err := s.AppendUpdate(ctx, "room", a)
	if err != nil || v1 != 1 {
		t.Fatalf("append1: v=%d err=%v want 1", v1, err)
	}
	v2, err := s.AppendUpdate(ctx, "room", b)
	if err != nil || v2 != 2 {
		t.Fatalf("append2: v=%d err=%v want 2", v2, err)
	}
	lr, err := s.Load(ctx, "room")
	if err != nil || lr.Version != 2 {
		t.Fatalf("load: v=%d err=%v want 2", lr.Version, err)
	}
	d := crdt.New()
	if err := crdt.ApplyUpdateV1(d, lr.Update, nil); err != nil {
		t.Fatalf("apply head: %v", err)
	}
	if got := d.GetText("t").ToString(); got != "ab" {
		t.Fatalf("head text = %q, want %q", got, "ab")
	}
}

func TestAppendUpdate_RejectsInvalid(t *testing.T) {
	s, _ := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	if _, err := s.AppendUpdate(context.Background(), "room", []byte{0xff, 0xff, 0xff}); err == nil {
		t.Fatal("expected error for invalid update, got nil")
	}
}

func TestListAndMaterialize(t *testing.T) {
	s, _ := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	ctx := context.Background()
	a, b := twoUpdates(t)
	_, _ = s.AppendUpdate(ctx, "room", a)
	_, _ = s.AppendUpdate(ctx, "room", b)

	vers, err := s.ListVersions(ctx, "room")
	if err != nil || len(vers) != 2 || vers[0].Version != 2 || vers[1].Version != 1 {
		t.Fatalf("ListVersions newest-first: %+v err=%v", vers, err)
	}
	if vers[0].UpdatedAt.IsZero() {
		t.Fatal("VersionMeta.UpdatedAt must be set")
	}

	if _, _, ok, _ := s.GetUpdate(ctx, "room", 2); !ok {
		t.Fatal("GetUpdate v2 ok=false")
	}
	if _, _, ok, _ := s.GetUpdate(ctx, "room", 9); ok {
		t.Fatal("GetUpdate v9 ok=true, want false")
	}

	empty, err := s.MaterializeAt(ctx, "room", 0)
	if err != nil || empty != nil {
		t.Fatalf("MaterializeAt v0: got %v err=%v, want nil/nil", empty, err)
	}
	if _, err := s.MaterializeAt(ctx, "missing", 5); err != persistence.ErrRoomNotFound {
		t.Fatalf("MaterializeAt missing room: err=%v want ErrRoomNotFound", err)
	}
}
