package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/persistence/sqlite"
)

// mustOpen opens a Store and fails the test immediately if Open errors, so an
// Open failure surfaces directly instead of as a later nil dereference.
func mustOpen(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(%q): %v", path, err)
	}
	return s
}

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
	s := mustOpen(t, filepath.Join(t.TempDir(), "t.db"))
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
	s := mustOpen(t, filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	if _, err := s.AppendUpdate(context.Background(), "room", []byte{0xff, 0xff, 0xff}); err == nil {
		t.Fatal("expected error for invalid update, got nil")
	}
}

func TestListAndMaterialize(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "t.db"))
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
	s := mustOpen(t, filepath.Join(t.TempDir(), "t.db"))
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
	s := mustOpen(t, path)
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

func TestCompact(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	ctx := context.Background()
	d := crdt.New(crdt.WithClientID(7))
	txt := d.GetText("t")
	var prevSV crdt.StateVector
	for i := 0; i < 4; i++ {
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
	textOf := func() string {
		lr, _ := s.Load(ctx, "room")
		dd := crdt.New()
		if err := crdt.ApplyUpdateV1(dd, lr.Update, nil); err != nil {
			t.Fatalf("apply: %v", err)
		}
		return dd.GetText("t").ToString()
	}
	want := textOf()

	deleted, err := s.Compact(ctx, "room", 2) // keep newest 2 (versions 3,4)
	if err != nil || deleted != 2 {
		t.Fatalf("Compact: deleted=%d err=%v want 2", deleted, err)
	}
	vers, _ := s.ListVersions(ctx, "room")
	if len(vers) != 2 || vers[0].Version != 4 || vers[1].Version != 3 {
		t.Fatalf("after compact versions = %+v, want [4,3]", vers)
	}
	if got := textOf(); got != want {
		t.Fatalf("compact changed materialized text: got %q want %q", got, want)
	}
	if n, _ := s.Compact(ctx, "room", 0); n != 0 {
		t.Fatalf("Compact keep=0 deleted=%d want 0", n)
	}
}

func TestConformance_SQLite(t *testing.T) {
	dir := t.TempDir()
	n := 0
	persistence.RunConformance(t, func() persistence.VersionedPersistence {
		n++
		s, err := sqlite.Open(filepath.Join(dir, fmt.Sprintf("conf-%d.db", n)))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s
	})
}

// TestOpen_URIPathWithExistingQuery proves Open does not emit a double-"?" DSN
// when the caller passes a URI-form path that already carries a query string.
// modernc accepts file: URIs; we append a pragma block with "&" rather than "?".
func TestOpen_URIPathWithExistingQuery(t *testing.T) {
	path := "file:" + filepath.Join(t.TempDir(), "u.db") + "?cache=shared"
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open URI with existing query: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	a, _ := twoUpdates(t)
	if _, err := s.AppendUpdate(ctx, "room", a); err != nil {
		t.Fatalf("AppendUpdate: %v", err)
	}
	lr, err := s.Load(ctx, "room")
	if err != nil || lr.Version != 1 {
		t.Fatalf("Load: v=%d err=%v want 1", lr.Version, err)
	}
}

// appendN appends n incremental V1 updates ("a","b","c",...) for one document
// at versions 1..n into room, returning the document so callers can inspect the
// final state. Mirrors the TestPruneCrashRecovery construction.
func appendN(t *testing.T, s *sqlite.Store, room string, n int) *crdt.Doc {
	t.Helper()
	ctx := context.Background()
	d := crdt.New(crdt.WithClientID(7))
	txt := d.GetText("t")
	var prevSV crdt.StateVector
	for i := 0; i < n; i++ {
		d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, string(rune('a'+i)), nil) })
		var upd []byte
		if i == 0 {
			upd = crdt.EncodeStateAsUpdateV1(d, nil)
		} else {
			upd = crdt.EncodeStateAsUpdateV1(d, prevSV)
		}
		if _, err := s.AppendUpdate(ctx, room, upd); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		prevSV = d.StateVector()
	}
	return d
}

// TestCompact_RespectsCanceledContext proves Compact checks ctx at entry, even
// on the keep<=0 fast path which previously returned (0, nil) unconditionally.
func TestCompact_RespectsCanceledContext(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Compact(ctx, "room", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Compact(keep=0) on canceled ctx: err=%v, want context.Canceled", err)
	}
	if _, err := s.Compact(ctx, "room", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("Compact(keep=5) on canceled ctx: err=%v, want context.Canceled", err)
	}
}

// textFromUpdate applies a V1 update and returns the resulting text of "t".
func textFromUpdate(t *testing.T, update []byte) string {
	t.Helper()
	d := crdt.New()
	if err := crdt.ApplyUpdateV1(d, update, nil); err != nil {
		t.Fatalf("apply update: %v", err)
	}
	return d.GetText("t").ToString()
}

// TestReadsClampToCheckpoint_AfterCrash locks the clamp/consistency invariant:
// with a checkpoint (target=2) committed but the future row (v3) still
// physically present (mid-prune crash), Load and MaterializeAt must clamp to the
// checkpoint ceiling — never merging v3 — and return a consistent {Version,
// Update} snapshot. Asserts WITHOUT reopening (the live handle must clamp too).
func TestReadsClampToCheckpoint_AfterCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s := mustOpen(t, path)
	defer s.Close()
	ctx := context.Background()

	appendN(t, s, "room", 3) // versions 1..3 => text "cba"

	// State of the document at version 2 (the checkpoint ceiling).
	at2, err := s.MaterializeAt(ctx, "room", 2)
	if err != nil {
		t.Fatalf("MaterializeAt v2 (pre-prune): %v", err)
	}
	want2 := textFromUpdate(t, at2)

	// Crash right after the checkpoint write: row v3 stays on disk, target=2.
	s.SetCrashAfterCheckpoint(func() bool { return true })
	if err := s.PruneAfter(ctx, "room", 2, at2); err != nil {
		t.Fatalf("PruneAfter (crashing): %v", err)
	}
	s.SetCrashAfterCheckpoint(nil)

	// Load must clamp to v2 and return a consistent snapshot for that version.
	lr, err := s.Load(ctx, "room")
	if err != nil {
		t.Fatalf("Load after crash: %v", err)
	}
	if lr.Version != 2 {
		t.Fatalf("Load clamped head = %d, want 2", lr.Version)
	}
	if got := textFromUpdate(t, lr.Update); got != want2 {
		t.Fatalf("Load update = %q, want version-2 state %q (must NOT include v3)", got, want2)
	}

	// MaterializeAt above the ceiling clamps to v2.
	at3, err := s.MaterializeAt(ctx, "room", 3)
	if err != nil {
		t.Fatalf("MaterializeAt v3 after crash: %v", err)
	}
	if got := textFromUpdate(t, at3); got != want2 {
		t.Fatalf("MaterializeAt v3 = %q, want clamped version-2 state %q", got, want2)
	}

	// MaterializeAt at the ceiling equals Load's update.
	at2b, err := s.MaterializeAt(ctx, "room", 2)
	if err != nil {
		t.Fatalf("MaterializeAt v2 after crash: %v", err)
	}
	if got := textFromUpdate(t, at2b); got != want2 {
		t.Fatalf("MaterializeAt v2 = %q, want %q", got, want2)
	}
}

// TestConcurrentLoadDuringPrune is a -race smoke test: a writer goroutine
// appends and occasionally prunes room "r" while a reader repeatedly Loads.
// Load must never error nor return a torn blob — every non-empty result must
// decode cleanly. Bounded for speed.
func TestConcurrentLoadDuringPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s := mustOpen(t, path)
	defer s.Close()

	const iters = 200
	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	// Seed one update so reads have something immediately.
	appendN(t, s, "r", 1)

	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		d := crdt.New(crdt.WithClientID(99))
		txt := d.GetText("t")
		prevSV := crdt.StateVector(nil)
		for i := 0; i < iters; i++ {
			d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "x", nil) })
			var upd []byte
			if prevSV == nil {
				upd = crdt.EncodeStateAsUpdateV1(d, nil)
			} else {
				upd = crdt.EncodeStateAsUpdateV1(d, prevSV)
			}
			prevSV = d.StateVector()
			v, err := s.AppendUpdate(ctx, "r", upd)
			if err != nil {
				errCh <- fmt.Errorf("AppendUpdate: %w", err)
				return
			}
			if i%7 == 6 && v > 1 {
				target := v - 1
				rolled, err := s.MaterializeAt(ctx, "r", target)
				if err != nil {
					errCh <- fmt.Errorf("MaterializeAt: %w", err)
					return
				}
				if err := s.PruneAfter(ctx, "r", target, rolled); err != nil {
					errCh <- fmt.Errorf("PruneAfter: %w", err)
					return
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		ctx := context.Background()
		for i := 0; i < iters; i++ {
			lr, err := s.Load(ctx, "r")
			if err != nil {
				errCh <- fmt.Errorf("Load: %w", err)
				return
			}
			if lr.Update == nil {
				continue
			}
			if err := crdt.ApplyUpdateV1(crdt.New(), lr.Update, nil); err != nil {
				errCh <- fmt.Errorf("Load returned undecodable blob at version %d: %w", lr.Version, err)
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestInMemoryMode(t *testing.T) {
	s, err := sqlite.Open("") // ephemeral
	if err != nil {
		t.Fatalf("Open in-memory: %v", err)
	}
	defer s.Close()
	a, _ := twoUpdates(t)
	if _, err := s.AppendUpdate(context.Background(), "r", a); err != nil {
		t.Fatalf("append in-memory: %v", err)
	}
	lr, _ := s.Load(context.Background(), "r")
	if lr.Version != 1 {
		t.Fatalf("in-memory load v=%d want 1", lr.Version)
	}
}
