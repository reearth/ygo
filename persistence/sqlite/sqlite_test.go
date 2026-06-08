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

func TestSnapshotsAndDelete(t *testing.T) {
	s, _ := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	ctx := context.Background()
	a, _ := twoUpdates(t)
	_, _ = s.AppendUpdate(ctx, "room", a)

	v, err := s.CaptureSnapshot(ctx, "room", "named", []byte("STATE"))
	if err != nil || v != 1 {
		t.Fatalf("CaptureSnapshot: v=%d err=%v want 1", v, err)
	}
	blob, sv, ok, err := s.RestoreSnapshot(ctx, "room", "named")
	if err != nil || !ok || sv != 1 || string(blob) != "STATE" {
		t.Fatalf("RestoreSnapshot: blob=%q v=%d ok=%v err=%v", blob, sv, ok, err)
	}
	if _, _, ok, _ := s.RestoreSnapshot(ctx, "room", "missing"); ok {
		t.Fatal("RestoreSnapshot missing: ok=true want false")
	}

	// Re-capturing the same (room, name) overwrites the prior blob (exercises the
	// ON CONFLICT DO UPDATE arm).
	if _, err := s.CaptureSnapshot(ctx, "room", "named", []byte("STATE2")); err != nil {
		t.Fatalf("CaptureSnapshot overwrite: %v", err)
	}
	blob2, _, ok2, err := s.RestoreSnapshot(ctx, "room", "named")
	if err != nil || !ok2 || string(blob2) != "STATE2" {
		t.Fatalf("RestoreSnapshot after overwrite: blob=%q ok=%v err=%v want STATE2", blob2, ok2, err)
	}

	if err := s.Delete(ctx, "room"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	lr, _ := s.Load(ctx, "room")
	if lr.Version != 0 {
		t.Fatalf("after Delete, Load v=%d want 0", lr.Version)
	}
	if _, _, ok, _ := s.RestoreSnapshot(ctx, "room", "named"); ok {
		t.Fatal("after Delete, snapshot still present")
	}
}

func TestPruneCrashRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, _ := sqlite.Open(path)
	defer s.Close()
	ctx := context.Background()

	// Append 3 updates (versions 1..3) for one doc.
	d := crdt.New(crdt.WithClientID(7))
	txt := d.GetText("t")
	var prevSV crdt.StateVector
	for i := 0; i < 3; i++ {
		d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, string(rune('a'+i)), nil) })
		var upd []byte
		if i == 0 {
			upd = crdt.EncodeStateAsUpdateV1(d, nil)
		} else {
			upd = crdt.EncodeStateAsUpdateV1(d, prevSV)
		}
		if _, err := s.AppendUpdate(ctx, "room", upd); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		prevSV = d.StateVector()
	}

	rolled, _ := s.MaterializeAt(ctx, "room", 2)

	// Crash right after the checkpoint write, before deletes.
	s.SetCrashAfterCheckpoint(func() bool { return true })
	if err := s.PruneAfter(ctx, "room", 2, rolled); err != nil {
		t.Fatalf("PruneAfter (crashing): %v", err)
	}
	s.SetCrashAfterCheckpoint(nil)

	// Reopen and assert no resurrected versions despite stale rows on disk.
	ro, _ := s.Reopen()
	r := ro.(*sqlite.Store)
	defer r.Close()
	vers, _ := r.ListVersions(ctx, "room")
	for _, m := range vers {
		if m.Version > 2 {
			t.Fatalf("resurrected version %d after mid-prune crash", m.Version)
		}
	}
	lr, _ := r.Load(ctx, "room")
	if lr.Version != 2 {
		t.Fatalf("post-crash head = %d, want 2", lr.Version)
	}
	// Recovery: next append finishes the prune and lands at 3.
	d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "z", nil) })
	recoveryUpd := crdt.EncodeStateAsUpdateV1(d, prevSV)
	nv, err := r.AppendUpdate(ctx, "room", recoveryUpd)
	if err != nil || nv != 3 {
		t.Fatalf("recovery append: v=%d err=%v want 3", nv, err)
	}
}
