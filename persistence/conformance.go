package persistence

import (
	"context"
	"strings"
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

// runCrashSafePrune appends 5 updates, materializes the rolled-back head at
// target, injects a crash between PruneAfter's checkpoint write and its deletes,
// "reopens" the store, and asserts no version > target survives and Load returns
// wantText at version <= target. Skips when the impl can't inject a crash.
func runCrashSafePrune(t *testing.T, factory func() VersionedPersistence, target Version, wantText string) {
	t.Helper()
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
	rolledBack, err := p.MaterializeAt(ctx, "room", target)
	if err != nil {
		t.Fatalf("MaterializeAt(%d): %v", target, err)
	}

	// Inject a crash right after the checkpoint write, before the deletes.
	ci.SetCrashAfterCheckpoint(func() bool { return true })
	if err := p.PruneAfter(ctx, "room", target, rolledBack); err != nil {
		t.Fatalf("PruneAfter (crashing): %v", err)
	}
	ci.SetCrashAfterCheckpoint(nil)

	// "Reopen" the store (file: re-read dir; memory: same handle).
	reopened := p
	if ro, ok := p.(Reopener); ok {
		r, rerr := ro.Reopen()
		if rerr != nil {
			t.Fatalf("Reopen: %v", rerr)
		}
		reopened = r
	}

	// Despite the simulated crash leaving future records physically behind, no
	// version > target may be visible: the checkpoint is a hard ceiling.
	metas, err := reopened.ListVersions(ctx, "room")
	if err != nil {
		t.Fatalf("ListVersions after crash: %v", err)
	}
	for _, m := range metas {
		if m.Version > target {
			t.Fatalf("RESURRECTED future version %d after mid-prune crash (target=%d)", m.Version, target)
		}
	}
	lr, err := reopened.Load(ctx, "room")
	if err != nil {
		t.Fatalf("Load after crash: %v", err)
	}
	if lr.Version > target {
		t.Fatalf("Load head %d > target %d after mid-prune crash", lr.Version, target)
	}
	if got := textOf(t, lr.Update); got != wantText {
		t.Fatalf("post-crash Load text = %q, want %q", got, wantText)
	}

	// Recovery: an append after the crashed prune must finish the interrupted
	// prune (drop the leaked future records) and then commit a fresh version.
	// The 5 original updates produced versions 1..5; everything > target leaked.
	fresh := markerUpdate(t, "Z")
	nv, err := reopened.AppendUpdate(ctx, "room", fresh)
	if err != nil {
		t.Fatalf("AppendUpdate after crashed prune: %v", err)
	}
	// Impl-agnostic: do NOT assert nv == target+1. Both impls densely reuse
	// target+1, but the contract only requires the new version exceeds target.
	if nv <= target {
		t.Fatalf("append after crashed prune returned version %d, want > target %d", nv, target)
	}
	lr2, err := reopened.Load(ctx, "room")
	if err != nil {
		t.Fatalf("Load after recovery append: %v", err)
	}
	if lr2.Version != nv {
		t.Fatalf("Load head = %d after recovery append, want %d", lr2.Version, nv)
	}
	// Content must be state-at-target (wantText) ⊕ the fresh marker, and must NOT
	// contain any leaked record content. The leaked updates 'c'/'d'/'e' (and 'a'
	// when target=0) inserted distinct runes; assert none resurfaced.
	got := textOf(t, lr2.Update)
	if !strings.Contains(got, "Z") {
		t.Fatalf("recovery Load text = %q, missing fresh marker %q", got, "Z")
	}
	if wantText != "" && !strings.Contains(got, wantText) {
		t.Fatalf("recovery Load text = %q, lost rolled-back head %q", got, wantText)
	}
	for _, leaked := range leakedRunes(target) {
		if strings.ContainsRune(got, leaked) {
			t.Fatalf("recovery Load text = %q resurrected leaked rune %q", got, string(leaked))
		}
	}
	// No leaked future version may be resurrected by GetUpdate, except the one
	// version the recovery append legitimately (densely) reused.
	for v := target + 1; v <= 5; v++ {
		if v == nv {
			continue // densely reused by the recovery append
		}
		_, _, ok, err := reopened.GetUpdate(ctx, "room", v)
		if err != nil {
			t.Fatalf("GetUpdate(%d) after recovery: %v", v, err)
		}
		if ok {
			t.Fatalf("GetUpdate(%d) resurrected a leaked version after recovery (target=%d, nv=%d)", v, target, nv)
		}
	}
}

// leakedRunes returns the inserted runes for the original updates with version
// > target, which a crashed-then-recovered prune must NOT resurface. genUpdates
// inserts rune 'a'+i at version i+1, so version v carries rune 'a'+(v-1).
func leakedRunes(target Version) []rune {
	var out []rune
	for v := target + 1; v <= 5; v++ {
		out = append(out, rune('a'+(v-1)))
	}
	return out
}

// markerUpdate builds a standalone well-formed V1 update on shared text "t" that
// inserts marker at index 0. It uses a distinct client ID so it merges cleanly
// onto any pruned/rolled-back head without colliding with genUpdates' client.
func markerUpdate(t *testing.T, marker string) []byte {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(9999))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, marker, nil) })
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// containsVersion reports whether metas includes version v.
func containsVersion(metas []VersionMeta, v Version) bool {
	for _, m := range metas {
		if m.Version == v {
			return true
		}
	}
	return false
}

// RunConformance runs the full VersionedPersistence behavioural suite against a
// fresh store produced by factory. External adapters import this package and
// call RunConformance with their own factory to verify conformance. The factory
// must return an empty store on each call.
func RunConformance(t *testing.T, factory func() VersionedPersistence) {
	t.Helper()

	t.Run("AppendAndListNewestFirst", func(t *testing.T) {
		p := factory()
		ctx := context.Background()
		updates, _ := genUpdates(t, 3)
		versions := make([]Version, 0, len(updates))
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
		t.Run("target=2", func(t *testing.T) {
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

			// An append after the prune must be visible — the prune must NOT
			// freeze the room at target. Append a fresh well-formed update that
			// inserts a marker, and assert it surfaces through Load/ListVersions.
			fresh := markerUpdate(t, "Z")
			nv, err := p.AppendUpdate(ctx, "room", fresh)
			if err != nil {
				t.Fatalf("AppendUpdate after prune: %v", err)
			}
			if nv <= 2 {
				t.Fatalf("append after prune returned version %d, want > target 2", nv)
			}
			lr2, err := p.Load(ctx, "room")
			if err != nil {
				t.Fatalf("Load after post-prune append: %v", err)
			}
			if lr2.Version != nv {
				t.Fatalf("Load head version = %d after post-prune append, want %d", lr2.Version, nv)
			}
			metas2, err := p.ListVersions(ctx, "room")
			if err != nil {
				t.Fatalf("ListVersions after post-prune append: %v", err)
			}
			if !containsVersion(metas2, nv) {
				t.Fatalf("ListVersions missing post-prune append version %d: %v", nv, metas2)
			}
			// The materialized head must reflect state-at-target ("ba") AND the
			// freshly appended marker, proving the append folded onto the
			// rolled-back head rather than being clamped away by the prune ceiling.
			got := textOf(t, lr2.Update)
			if !strings.Contains(got, "ba") {
				t.Fatalf("post-prune append Load text = %q, lost rolled-back head %q", got, "ba")
			}
			if !strings.Contains(got, "Z") {
				t.Fatalf("post-prune append Load text = %q, missing fresh marker %q", got, "Z")
			}
		})

		// target=0 is the boundary case the checkpoint-zero overload broke:
		// pruning to the empty head must remove ALL versions and leave Load
		// returning the empty/rolled-back head with version 0.
		t.Run("target=0", func(t *testing.T) {
			p := factory()
			ctx := context.Background()
			updates, _ := genUpdates(t, 5)
			for _, u := range updates {
				if _, err := p.AppendUpdate(ctx, "room", u); err != nil {
					t.Fatalf("AppendUpdate: %v", err)
				}
			}
			rolledBack, err := p.MaterializeAt(ctx, "room", 0) // empty head
			if err != nil {
				t.Fatalf("MaterializeAt(0): %v", err)
			}
			if err := p.PruneAfter(ctx, "room", 0, rolledBack); err != nil {
				t.Fatalf("PruneAfter(0): %v", err)
			}
			metas, err := p.ListVersions(ctx, "room")
			if err != nil {
				t.Fatalf("ListVersions: %v", err)
			}
			if len(metas) != 0 {
				t.Fatalf("PruneAfter(0) left %d versions, want 0", len(metas))
			}
			lr, err := p.Load(ctx, "room")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if lr.Version != 0 {
				t.Fatalf("post-prune(0) Load version = %d, want 0", lr.Version)
			}
			if got := textOf(t, lr.Update); got != "" {
				t.Fatalf("post-prune(0) Load text = %q, want empty", got)
			}
		})
	})

	t.Run("PruneAfterCrashSafe_NoSpuriousFutureVersions", func(t *testing.T) {
		// target=2: a crash mid-prune must not resurrect versions 3,4,5; head
		// stays at the rolled-back text "ba".
		t.Run("target=2", func(t *testing.T) {
			runCrashSafePrune(t, factory, 2, "ba")
		})
		// target=0: a crash mid-prune-to-empty must NOT resurrect ANY version
		// (1..5); the head is the empty rolled-back state at version 0.
		t.Run("target=0", func(t *testing.T) {
			runCrashSafePrune(t, factory, 0, "")
		})
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
