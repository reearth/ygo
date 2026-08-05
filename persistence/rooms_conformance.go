package persistence

import (
	"context"
	"sort"
	"testing"
)

// RunRoomListerConformance exercises the RoomLister contract against a backend.
// factory must return a fresh, empty store per call that implements both
// VersionedPersistence (to create data) and RoomLister (to enumerate it).
//
// It lives in the non-test build (like RunConformance) so backends in other
// packages can call it.
func RunRoomListerConformance(t *testing.T, factory func() SnapshotVersionedPersistence) {
	t.Helper()
	ctx := context.Background()

	// listed returns the store's rooms, sorted (ListRooms order is unspecified).
	listed := func(t *testing.T, p SnapshotVersionedPersistence) []string {
		t.Helper()
		l, ok := p.(RoomLister)
		if !ok {
			t.Fatalf("%T does not implement RoomLister", p)
		}
		got, err := l.ListRooms(ctx)
		if err != nil {
			t.Fatalf("ListRooms: %v", err)
		}
		sort.Strings(got)
		return got
	}

	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	t.Run("EmptyStoreListsNothing", func(t *testing.T) {
		p := factory()
		if got := listed(t, p); len(got) != 0 {
			t.Fatalf("ListRooms on empty store = %v, want none", got)
		}
	})

	t.Run("ListsRoomsWithUpdates", func(t *testing.T) {
		p := factory()
		updates, _ := genUpdates(t, 1)
		for _, room := range []string{"beta", "alpha"} {
			if _, err := p.AppendUpdate(ctx, room, updates[0]); err != nil {
				t.Fatalf("AppendUpdate(%s): %v", room, err)
			}
		}
		want := []string{"alpha", "beta"}
		if got := listed(t, p); !equal(got, want) {
			t.Fatalf("ListRooms = %v, want %v", got, want)
		}
	})

	t.Run("ListsRoomWithOnlyASnapshot", func(t *testing.T) {
		// A room that has a snapshot but no update log must still be reported,
		// otherwise a cleanup pass would never see it and its snapshot would be
		// unreclaimable.
		p := factory()
		if _, err := p.SaveSnapshot(ctx, "snaponly", "lbl", []byte("state")); err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
		want := []string{"snaponly"}
		if got := listed(t, p); !equal(got, want) {
			t.Fatalf("ListRooms = %v, want %v", got, want)
		}
	})

	t.Run("NoDuplicatesWhenRoomHasBothUpdatesAndSnapshots", func(t *testing.T) {
		p := factory()
		updates, _ := genUpdates(t, 2)
		for _, u := range updates {
			if _, err := p.AppendUpdate(ctx, "both", u); err != nil {
				t.Fatalf("AppendUpdate: %v", err)
			}
		}
		if _, err := p.SaveSnapshot(ctx, "both", "a", []byte("s1")); err != nil {
			t.Fatalf("SaveSnapshot #1: %v", err)
		}
		if _, err := p.SaveSnapshot(ctx, "both", "b", []byte("s2")); err != nil {
			t.Fatalf("SaveSnapshot #2: %v", err)
		}
		want := []string{"both"}
		if got := listed(t, p); !equal(got, want) {
			t.Fatalf("ListRooms = %v, want exactly %v (no duplicates)", got, want)
		}
	})

	t.Run("DeletedRoomIsNotListed", func(t *testing.T) {
		p := factory()
		updates, _ := genUpdates(t, 1)
		for _, room := range []string{"keep", "drop"} {
			if _, err := p.AppendUpdate(ctx, room, updates[0]); err != nil {
				t.Fatalf("AppendUpdate(%s): %v", room, err)
			}
		}
		if _, err := p.SaveSnapshot(ctx, "drop", "lbl", []byte("state")); err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
		if err := p.Delete(ctx, "drop"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		want := []string{"keep"}
		if got := listed(t, p); !equal(got, want) {
			t.Fatalf("ListRooms after Delete = %v, want %v", got, want)
		}
	})

	t.Run("RoomNamesRoundTripExactly", func(t *testing.T) {
		// Backends may encode room names on disk (the file backend hex-encodes
		// them); enumeration must return the ORIGINAL name, not the encoded form.
		p := factory()
		names := conformanceRoomNames
		updates, _ := genUpdates(t, 1)
		for _, n := range names {
			if _, err := p.AppendUpdate(ctx, n, updates[0]); err != nil {
				t.Fatalf("AppendUpdate(%q): %v", n, err)
			}
		}
		want := append([]string(nil), names...)
		sort.Strings(want)
		if got := listed(t, p); !equal(got, want) {
			t.Fatalf("ListRooms = %v, want %v", got, want)
		}
	})
}
