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
