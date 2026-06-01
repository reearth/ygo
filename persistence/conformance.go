package persistence

import (
	"context"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// CrashInjector is an optional interface a VersionedPersistence implementation
// may satisfy to let the conformance suite simulate a crash partway through
// PruneAfter (between the checkpoint write and the deletes). Implementations
// that cannot simulate this should not implement it; the suite skips the
// crash-safety subtest for them (and logs a notice).
type CrashInjector interface {
	// SetCrashAfterCheckpoint installs a hook PruneAfter calls immediately after
	// writing its checkpoint + rolled-back head but before deleting future
	// records. Returning true makes PruneAfter return early, leaving stale
	// future records behind. Pass nil to clear.
	SetCrashAfterCheckpoint(fn func() bool)
}

// Reopener is an optional interface a VersionedPersistence may satisfy to model
// a process restart: it returns a fresh handle over the same backing store.
// File-backed stores reopen the directory; in-memory stores return themselves
// (the data already survives within the process). Used by the crash-safety
// subtest to assert that a crash-left store does not resurrect future versions
// after "reopen".
type Reopener interface {
	Reopen() (VersionedPersistence, error)
}

// genUpdates produces n sequential incremental V1 updates from a single text
// document, returning the updates and the expected text after applying each.
// Update i inserts the rune ('a'+i) at index 0, so the doc reads in reverse
// insert order.
func genUpdates(t *testing.T, n int) (updates [][]byte, finalText string) {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(7))
	txt := doc.GetText("t")
	var prev []byte
	for i := 0; i < n; i++ {
		ch := string(rune('a' + i))
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, ch, nil) })
		full := crdt.EncodeStateAsUpdateV1(doc, nil)
		var inc []byte
		if prev == nil {
			inc = full
		} else {
			d, err := crdt.DiffUpdateV1(full, mustStateVector(t, prev))
			if err != nil {
				t.Fatalf("DiffUpdateV1: %v", err)
			}
			inc = d
		}
		updates = append(updates, inc)
		prev = full
	}
	return updates, txt.ToString()
}

// mustStateVector returns the state vector of a doc reconstructed from a full V1
// blob.
func mustStateVector(t *testing.T, full []byte) crdt.StateVector {
	t.Helper()
	d := crdt.New()
	if err := crdt.ApplyUpdateV1(d, full, nil); err != nil {
		t.Fatalf("ApplyUpdateV1: %v", err)
	}
	return d.StateVector()
}

// applyAll merges blobs and returns the resulting text in shared type "t".
func textOf(t *testing.T, v1 []byte) string {
	t.Helper()
	if len(v1) == 0 {
		return ""
	}
	d := crdt.New()
	if err := crdt.ApplyUpdateV1(d, v1, nil); err != nil {
		t.Fatalf("ApplyUpdateV1: %v", err)
	}
	return d.GetText("t").ToString()
}

// RunConformance runs the full VersionedPersistence behavioural suite against a
// fresh store produced by factory. External adapters (e.g. a GCS-backed store)
// import this package and call RunConformance with their own factory to verify
// conformance. The factory must return an empty store on each call.
func RunConformance(t *testing.T, factory func() VersionedPersistence) {
	t.Helper()

	t.Run("AppendAndListNewestFirst", func(t *testing.T) {
		p := factory()
		ctx := context.Background()
		updates, _ := genUpdates(t, 3)
		var versions []Version
		for _, u := range updates {
			v, err := p.AppendUpdate(ctx, "room", u)
			if err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
			versions = append(versions, v)
		}
		if versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
			t.Fatalf("expected dense versions 1,2,3 got %v", versions)
		}
		metas, err := p.ListVersions(ctx, "room")
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		if len(metas) != 3 {
			t.Fatalf("expected 3 versions, got %d", len(metas))
		}
		if metas[0].Version != 3 || metas[1].Version != 2 || metas[2].Version != 1 {
			t.Fatalf("expected newest-first 3,2,1 got %d,%d,%d", metas[0].Version, metas[1].Version, metas[2].Version)
		}
	})

	t.Run("ListVersionsUnknownRoomEmpty", func(t *testing.T) {
		p := factory()
		metas, err := p.ListVersions(context.Background(), "nope")
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		if len(metas) != 0 {
			t.Fatalf("expected empty, got %d", len(metas))
		}
	})

	t.Run("GetUpdate", func(t *testing.T) {
		p := factory()
		ctx := context.Background()
		updates, _ := genUpdates(t, 2)
		for _, u := range updates {
			if _, err := p.AppendUpdate(ctx, "room", u); err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
		}
		got, meta, ok, err := p.GetUpdate(ctx, "room", 2)
		if err != nil || !ok {
			t.Fatalf("GetUpdate(2): ok=%v err=%v", ok, err)
		}
		if meta.Version != 2 {
			t.Fatalf("meta.Version = %d, want 2", meta.Version)
		}
		if len(got) == 0 {
			t.Fatalf("GetUpdate returned empty payload")
		}
		// Missing version.
		_, _, ok, err = p.GetUpdate(ctx, "room", 99)
		if err != nil {
			t.Fatalf("GetUpdate(99) err: %v", err)
		}
		if ok {
			t.Fatalf("GetUpdate(99) should be ok=false")
		}
	})

	t.Run("MaterializeAtRebuildsState", func(t *testing.T) {
		p := factory()
		ctx := context.Background()
		updates, finalText := genUpdates(t, 3) // text == "cba"
		for _, u := range updates {
			if _, err := p.AppendUpdate(ctx, "room", u); err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
		}
		// At v1 only the first insert ("a") is present.
		at1, err := p.MaterializeAt(ctx, "room", 1)
		if err != nil {
			t.Fatalf("MaterializeAt(1): %v", err)
		}
		if got := textOf(t, at1); got != "a" {
			t.Fatalf("MaterializeAt(1) text = %q, want %q", got, "a")
		}
		// At head, full text.
		at3, err := p.MaterializeAt(ctx, "room", 3)
		if err != nil {
			t.Fatalf("MaterializeAt(3): %v", err)
		}
		if got := textOf(t, at3); got != finalText {
			t.Fatalf("MaterializeAt(3) text = %q, want %q", got, finalText)
		}
		// Load head matches MaterializeAt(head).
		lr, err := p.Load(ctx, "room")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if lr.Version != 3 {
			t.Fatalf("Load head version = %d, want 3", lr.Version)
		}
		if got := textOf(t, lr.Update); got != finalText {
			t.Fatalf("Load text = %q, want %q", got, finalText)
		}
		// v0 materializes to empty.
		at0, err := p.MaterializeAt(ctx, "room", 0)
		if err != nil {
			t.Fatalf("MaterializeAt(0): %v", err)
		}
		if textOf(t, at0) != "" {
			t.Fatalf("MaterializeAt(0) should be empty")
		}
	})

	t.Run("LoadUnknownRoomEmpty", func(t *testing.T) {
		p := factory()
		lr, err := p.Load(context.Background(), "nope")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if lr.Version != 0 || len(lr.Update) != 0 {
			t.Fatalf("expected zero LoadResult, got %+v", lr)
		}
	})

	t.Run("PruneAfterRemovesFutureVersions", func(t *testing.T) {
		p := factory()
		ctx := context.Background()
		updates, _ := genUpdates(t, 5)
		for _, u := range updates {
			if _, err := p.AppendUpdate(ctx, "room", u); err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
		}
		rolledBack, err := p.MaterializeAt(ctx, "room", 2)
		if err != nil {
			t.Fatalf("MaterializeAt(2): %v", err)
		}
		if err := p.PruneAfter(ctx, "room", 2, rolledBack); err != nil {
			t.Fatalf("PruneAfter: %v", err)
		}
		metas, err := p.ListVersions(ctx, "room")
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		for _, m := range metas {
			if m.Version > 2 {
				t.Fatalf("PruneAfter(2) left future version %d", m.Version)
			}
		}
		// Load head must reflect target state (text "ba" after inserts a,b).
		lr, err := p.Load(ctx, "room")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if lr.Version > 2 {
			t.Fatalf("Load head version %d > target 2 after prune", lr.Version)
		}
		if got := textOf(t, lr.Update); got != "ba" {
			t.Fatalf("post-prune Load text = %q, want %q", got, "ba")
		}
	})

	t.Run("PruneAfterCrashSafe_NoSpuriousFutureVersions", func(t *testing.T) {
		p := factory()
		ci, canInject := p.(CrashInjector)
		if !canInject {
			t.Skip("implementation does not support crash injection; skipping crash-safety subtest")
		}
		ctx := context.Background()
		updates, _ := genUpdates(t, 5)
		for _, u := range updates {
			if _, err := p.AppendUpdate(ctx, "room", u); err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
		}
		rolledBack, err := p.MaterializeAt(ctx, "room", 2)
		if err != nil {
			t.Fatalf("MaterializeAt(2): %v", err)
		}

		// Inject a crash right after the checkpoint write, before the deletes.
		ci.SetCrashAfterCheckpoint(func() bool { return true })
		if err := p.PruneAfter(ctx, "room", 2, rolledBack); err != nil {
			t.Fatalf("PruneAfter (crashing): %v", err)
		}
		ci.SetCrashAfterCheckpoint(nil)

		// "Reopen" the store (file: re-read dir; memory: same handle).
		reopened := p
		if ro, ok := p.(Reopener); ok {
			r, err := ro.Reopen()
			if err != nil {
				t.Fatalf("Reopen: %v", err)
			}
			reopened = r
		}

		// Despite the simulated crash leaving versions 3,4,5 physically behind,
		// no future version may be visible: the checkpoint is a hard ceiling.
		metas, err := reopened.ListVersions(ctx, "room")
		if err != nil {
			t.Fatalf("ListVersions after crash: %v", err)
		}
		for _, m := range metas {
			if m.Version > 2 {
				t.Fatalf("RESURRECTED future version %d after mid-prune crash", m.Version)
			}
		}
		lr, err := reopened.Load(ctx, "room")
		if err != nil {
			t.Fatalf("Load after crash: %v", err)
		}
		if lr.Version > 2 {
			t.Fatalf("Load head %d > target 2 after mid-prune crash", lr.Version)
		}
		if got := textOf(t, lr.Update); got != "ba" {
			t.Fatalf("post-crash Load text = %q, want %q", got, "ba")
		}
	})

	t.Run("CompactTrimsOldest", func(t *testing.T) {
		p := factory()
		ctx := context.Background()
		updates, finalText := genUpdates(t, 5)
		for _, u := range updates {
			if _, err := p.AppendUpdate(ctx, "room", u); err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
		}
		deleted, err := p.Compact(ctx, "room", 2)
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		if deleted != 3 {
			t.Fatalf("Compact(keep=2) deleted = %d, want 3", deleted)
		}
		metas, err := p.ListVersions(ctx, "room")
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		if len(metas) != 2 {
			t.Fatalf("after compact expected 2 records, got %d", len(metas))
		}
		// Materialized head state must be unchanged by compaction.
		lr, err := p.Load(ctx, "room")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := textOf(t, lr.Update); got != finalText {
			t.Fatalf("post-compact Load text = %q, want %q", got, finalText)
		}
	})

	t.Run("CaptureRestoreSnapshotRoundTrip", func(t *testing.T) {
		p := factory()
		ctx := context.Background()
		updates, finalText := genUpdates(t, 3)
		for _, u := range updates {
			if _, err := p.AppendUpdate(ctx, "room", u); err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
		}
		head, err := p.MaterializeAt(ctx, "room", 3)
		if err != nil {
			t.Fatalf("MaterializeAt: %v", err)
		}
		v, err := p.CaptureSnapshot(ctx, "room", "snap1", head)
		if err != nil {
			t.Fatalf("CaptureSnapshot: %v", err)
		}
		if v != 3 {
			t.Fatalf("snapshot version = %d, want 3", v)
		}
		got, gv, ok, err := p.RestoreSnapshot(ctx, "room", "snap1")
		if err != nil || !ok {
			t.Fatalf("RestoreSnapshot: ok=%v err=%v", ok, err)
		}
		if gv != 3 {
			t.Fatalf("restored version = %d, want 3", gv)
		}
		if txt := textOf(t, got); txt != finalText {
			t.Fatalf("restored snapshot text = %q, want %q", txt, finalText)
		}
		// Missing snapshot.
		_, _, ok, err = p.RestoreSnapshot(ctx, "room", "nope")
		if err != nil {
			t.Fatalf("RestoreSnapshot(nope) err: %v", err)
		}
		if ok {
			t.Fatalf("RestoreSnapshot(nope) should be ok=false")
		}
	})

	t.Run("DeleteRemovesRoom", func(t *testing.T) {
		p := factory()
		ctx := context.Background()
		updates, _ := genUpdates(t, 2)
		for _, u := range updates {
			if _, err := p.AppendUpdate(ctx, "room", u); err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
		}
		if err := p.Delete(ctx, "room"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		metas, err := p.ListVersions(ctx, "room")
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		if len(metas) != 0 {
			t.Fatalf("after delete expected 0 records, got %d", len(metas))
		}
		lr, err := p.Load(ctx, "room")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if lr.Version != 0 {
			t.Fatalf("after delete Load version = %d, want 0", lr.Version)
		}
	})
}
