package client

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func testUpdate(t *testing.T, s string) []byte {
	t.Helper()
	d := crdt.New()
	txt := d.GetText("t")
	d.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, s, nil) })
	return crdt.EncodeStateAsUpdateV1(d, nil)
}

// The default store round-trips updates across a reopen: what StoreUpdate
// accepted, LoadDoc returns merged, from a fresh handle on the same path —
// restart durability is the store's entire reason to exist.
func TestSQLiteStore_RoundTripAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreUpdate("room", testUpdate(t, "hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	blob, err := s2.LoadDoc("room")
	if err != nil {
		t.Fatal(err)
	}
	d := crdt.New()
	if err := crdt.ApplyUpdateV1(d, blob, nil); err != nil {
		t.Fatal(err)
	}
	if got := d.GetText("t").ToString(); got != "hello" {
		t.Fatalf("reloaded text = %q, want %q", got, "hello")
	}
}

// TestOpenSQLiteStore_DefaultsToNonZeroKeepVersions pins down the actual root
// cause #165 Task 1's review flagged: persistence.LegacyAdapter.KeepVersions
// defaults to 0 ("keep all history") when built via
// persistence.NewLegacyAdapter directly, and 0 makes every Compact call a
// permanent no-op (see LegacyAdapter.Compact's doc: "0 = keep all"). A store
// built via OpenSQLiteStore must NOT inherit that default silently — a
// caller who never thinks about retention at all (the common case: just
// call OpenSQLiteStore(path) and hand it to Options.Store) should still get
// working compaction once the client's compaction trigger (Client.
// maybeCompact) starts calling it, not a Compact that has quietly never
// deleted anything since the store was opened. See defaultKeepVersions' own
// doc for why this specific value was chosen.
func TestOpenSQLiteStore_DefaultsToNonZeroKeepVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.KeepVersions <= 0 {
		t.Fatalf("OpenSQLiteStore's KeepVersions = %d, want > 0: with the "+
			"LegacyAdapter default of 0 ('keep all'), Compact silently deletes "+
			"nothing, ever", s.KeepVersions)
	}
}

// Compact must not lose state: load-after-compact equals load-before.
func TestSQLiteStore_CompactPreservesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, w := range []string{"a", "b", "c"} {
		if err := s.StoreUpdate("room", testUpdate(t, w)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.LoadDoc("room")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(context.Background(), "room"); err != nil {
		t.Fatal(err)
	}
	after, err := s.LoadDoc("room")
	if err != nil {
		t.Fatal(err)
	}
	db, da := crdt.New(), crdt.New()
	_ = crdt.ApplyUpdateV1(db, before, nil)
	_ = crdt.ApplyUpdateV1(da, after, nil)
	if db.GetText("t").ToString() != da.GetText("t").ToString() {
		t.Fatal("Compact changed the loaded state")
	}
}

// TestSQLiteStore_CompactActuallyDeletesRows is the corrected assertion
// TestSQLiteStore_CompactPreservesState's own doc now points to: that test
// alone cannot tell "Compact deleted the trimmed prefix, folding it into the
// oldest retained row" apart from "Compact silently kept everything and did
// nothing" — both leave load-after equal to load-before, which is ALL that
// test checks. #165 Task 1's review caught exactly this gap:
// persistence.LegacyAdapter.KeepVersions defaults to 0 ("keep all"), so
// against an adapter built the default way Compact is a no-op that this
// package's own tests were not distinguishing from a working one.
//
// OpenSQLiteStore now sets a non-zero KeepVersions itself (see
// defaultKeepVersions), so the default path already exercises real deletion;
// this test pins the retained COUNT down explicitly by overriding it further,
// so the assertion is exact rather than "some default happened to be small
// enough."
func TestSQLiteStore_CompactActuallyDeletesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.KeepVersions = 2 // override OpenSQLiteStore's own default for an exact count below

	words := []string{"a", "b", "c", "d", "e"}
	for _, w := range words {
		if err := s.StoreUpdate("room", testUpdate(t, w)); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	before, err := s.Store().ListVersions(ctx, "room")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(words) {
		t.Fatalf("stored update rows before Compact = %d, want %d", len(before), len(words))
	}

	beforeState, err := s.LoadDoc("room")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(ctx, "room"); err != nil {
		t.Fatal(err)
	}

	after, err := s.Store().ListVersions(ctx, "room")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != s.KeepVersions {
		t.Fatalf("stored update rows after Compact = %d, want exactly KeepVersions=%d "+
			"(Compact must have genuinely deleted the trimmed prefix, not left everything in place)",
			len(after), s.KeepVersions)
	}

	afterState, err := s.LoadDoc("room")
	if err != nil {
		t.Fatal(err)
	}
	db, da := crdt.New(), crdt.New()
	if err := crdt.ApplyUpdateV1(db, beforeState, nil); err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdateV1(da, afterState, nil); err != nil {
		t.Fatal(err)
	}
	if db.GetText("t").ToString() != da.GetText("t").ToString() {
		t.Fatal("Compact changed the loaded state while deleting rows")
	}
}
