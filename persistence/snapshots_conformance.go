package persistence

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// RunSnapshotStoreConformance exercises the SnapshotStore contract against a
// backend. factory must return a fresh, empty store per call.
//
// It lives in the non-test build (like RunConformance) so backends in other
// packages can call it.
func RunSnapshotStoreConformance(t *testing.T, factory func() SnapshotStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("SaveThenListNewestFirst", func(t *testing.T) {
		s := factory()
		id1, err := s.SaveSnapshot(ctx, "room", "first", []byte("state-1"))
		if err != nil {
			t.Fatalf("SaveSnapshot(first): %v", err)
		}
		id2, err := s.SaveSnapshot(ctx, "room", "second", []byte("state-22"))
		if err != nil {
			t.Fatalf("SaveSnapshot(second): %v", err)
		}
		if id1 == id2 {
			t.Fatalf("ids must be distinct, both = %d", id1)
		}
		if id2 <= id1 {
			t.Fatalf("ids must increase: id1=%d id2=%d", id1, id2)
		}

		got, err := s.ListSnapshots(ctx, "room")
		if err != nil {
			t.Fatalf("ListSnapshots: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len(ListSnapshots) = %d, want 2", len(got))
		}
		// Newest first.
		if got[0].ID != id2 || got[1].ID != id1 {
			t.Fatalf("order = [%d %d], want newest-first [%d %d]", got[0].ID, got[1].ID, id2, id1)
		}
		if got[0].Label != "second" || got[1].Label != "first" {
			t.Fatalf("labels = [%q %q], want [second first]", got[0].Label, got[1].Label)
		}
		if got[0].Size != int64(len("state-22")) {
			t.Fatalf("Size = %d, want %d", got[0].Size, len("state-22"))
		}
		if got[0].CreatedAt.IsZero() || got[1].CreatedAt.IsZero() {
			t.Fatalf("CreatedAt must be stamped, got %v and %v", got[0].CreatedAt, got[1].CreatedAt)
		}
	})

	t.Run("ListUnknownRoomEmpty", func(t *testing.T) {
		s := factory()
		got, err := s.ListSnapshots(ctx, "nope")
		if err != nil {
			t.Fatalf("ListSnapshots(unknown): %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("len = %d, want 0", len(got))
		}
	})

	t.Run("GetSnapshotStateRoundTrip", func(t *testing.T) {
		s := factory()
		want := []byte("the-state-blob")
		id, err := s.SaveSnapshot(ctx, "room", "lbl", want)
		if err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
		got, err := s.GetSnapshotState(ctx, "room", id)
		if err != nil {
			t.Fatalf("GetSnapshotState: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("state = %q, want %q", got, want)
		}
	})

	t.Run("GetSnapshotStateUnknownIsErrSnapshotNotFound", func(t *testing.T) {
		s := factory()
		if _, err := s.GetSnapshotState(ctx, "room", 999); !errors.Is(err, ErrSnapshotNotFound) {
			t.Fatalf("err = %v, want ErrSnapshotNotFound", err)
		}
		// Also for a room that exists but a bogus id.
		id, err := s.SaveSnapshot(ctx, "room", "x", []byte("s"))
		if err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
		if _, err := s.GetSnapshotState(ctx, "room", id+12345); !errors.Is(err, ErrSnapshotNotFound) {
			t.Fatalf("err = %v, want ErrSnapshotNotFound", err)
		}
	})

	t.Run("SameLabelCreatesDistinctSnapshots", func(t *testing.T) {
		s := factory()
		id1, err := s.SaveSnapshot(ctx, "room", "dup", []byte("a"))
		if err != nil {
			t.Fatalf("SaveSnapshot #1: %v", err)
		}
		id2, err := s.SaveSnapshot(ctx, "room", "dup", []byte("bb"))
		if err != nil {
			t.Fatalf("SaveSnapshot #2: %v", err)
		}
		if id1 == id2 {
			t.Fatalf("same label must not overwrite: both ids = %d", id1)
		}
		got, err := s.ListSnapshots(ctx, "room")
		if err != nil {
			t.Fatalf("ListSnapshots: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2 (same label must not overwrite)", len(got))
		}
		// Both states must be independently retrievable.
		b1, err := s.GetSnapshotState(ctx, "room", id1)
		if err != nil {
			t.Fatalf("GetSnapshotState(id1): %v", err)
		}
		b2, err := s.GetSnapshotState(ctx, "room", id2)
		if err != nil {
			t.Fatalf("GetSnapshotState(id2): %v", err)
		}
		if !bytes.Equal(b1, []byte("a")) || !bytes.Equal(b2, []byte("bb")) {
			t.Fatalf("states = %q / %q, want a / bb", b1, b2)
		}
	})

	t.Run("EmptyLabelAllowed", func(t *testing.T) {
		s := factory()
		id, err := s.SaveSnapshot(ctx, "room", "", []byte("s"))
		if err != nil {
			t.Fatalf("SaveSnapshot(empty label): %v", err)
		}
		got, err := s.ListSnapshots(ctx, "room")
		if err != nil {
			t.Fatalf("ListSnapshots: %v", err)
		}
		if len(got) != 1 || got[0].ID != id || got[0].Label != "" {
			t.Fatalf("got %+v, want one entry id=%d label=%q", got, id, "")
		}
	})

	t.Run("EmptyStateRejected", func(t *testing.T) {
		s := factory()
		if _, err := s.SaveSnapshot(ctx, "room", "lbl", nil); !errors.Is(err, ErrEmptySnapshot) {
			t.Fatalf("SaveSnapshot(nil) err = %v, want ErrEmptySnapshot", err)
		}
		if _, err := s.SaveSnapshot(ctx, "room", "lbl", []byte{}); !errors.Is(err, ErrEmptySnapshot) {
			t.Fatalf("SaveSnapshot(empty) err = %v, want ErrEmptySnapshot", err)
		}
	})

	t.Run("DeleteSnapshotRemovesOnlyThatOne", func(t *testing.T) {
		s := factory()
		keep, err := s.SaveSnapshot(ctx, "room", "keep", []byte("k"))
		if err != nil {
			t.Fatalf("SaveSnapshot(keep): %v", err)
		}
		drop, err := s.SaveSnapshot(ctx, "room", "drop", []byte("d"))
		if err != nil {
			t.Fatalf("SaveSnapshot(drop): %v", err)
		}
		if err := s.DeleteSnapshot(ctx, "room", drop); err != nil {
			t.Fatalf("DeleteSnapshot: %v", err)
		}
		got, err := s.ListSnapshots(ctx, "room")
		if err != nil {
			t.Fatalf("ListSnapshots: %v", err)
		}
		if len(got) != 1 || got[0].ID != keep {
			t.Fatalf("after delete got %+v, want only id=%d", got, keep)
		}
		if _, err := s.GetSnapshotState(ctx, "room", drop); !errors.Is(err, ErrSnapshotNotFound) {
			t.Fatalf("deleted snapshot err = %v, want ErrSnapshotNotFound", err)
		}
		if _, err := s.GetSnapshotState(ctx, "room", keep); err != nil {
			t.Fatalf("kept snapshot must survive: %v", err)
		}
	})

	t.Run("DeleteSnapshotIdempotent", func(t *testing.T) {
		s := factory()
		if err := s.DeleteSnapshot(ctx, "room", 4242); err != nil {
			t.Fatalf("DeleteSnapshot(unknown) = %v, want nil", err)
		}
		id, err := s.SaveSnapshot(ctx, "room", "x", []byte("s"))
		if err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
		if err := s.DeleteSnapshot(ctx, "room", id); err != nil {
			t.Fatalf("DeleteSnapshot #1: %v", err)
		}
		if err := s.DeleteSnapshot(ctx, "room", id); err != nil {
			t.Fatalf("DeleteSnapshot #2 (idempotent) = %v, want nil", err)
		}
	})

	t.Run("IDsNotReusedAfterDelete", func(t *testing.T) {
		s := factory()
		id1, err := s.SaveSnapshot(ctx, "room", "a", []byte("a"))
		if err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
		if err := s.DeleteSnapshot(ctx, "room", id1); err != nil {
			t.Fatalf("DeleteSnapshot: %v", err)
		}
		id2, err := s.SaveSnapshot(ctx, "room", "b", []byte("b"))
		if err != nil {
			t.Fatalf("SaveSnapshot after delete: %v", err)
		}
		if id2 == id1 {
			t.Fatalf("id %d reused after delete", id2)
		}
	})

	// Delete(room) is contracted to remove ALL data for a room, snapshots
	// included. Skipped for a store that is snapshots-only.
	t.Run("RoomDeleteRemovesSnapshots", func(t *testing.T) {
		s := factory()
		type roomDeleter interface {
			Delete(ctx context.Context, room string) error
		}
		d, ok := s.(roomDeleter)
		if !ok {
			t.Skip("store does not implement Delete(ctx, room)")
		}
		id, err := s.SaveSnapshot(ctx, "room", "lbl", []byte("state"))
		if err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
		if err := d.Delete(ctx, "room"); err != nil {
			t.Fatalf("Delete(room): %v", err)
		}
		got, err := s.ListSnapshots(ctx, "room")
		if err != nil {
			t.Fatalf("ListSnapshots after Delete: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("after Delete(room) snapshots = %+v, want none", got)
		}
		if _, err := s.GetSnapshotState(ctx, "room", id); !errors.Is(err, ErrSnapshotNotFound) {
			t.Fatalf("after Delete(room) err = %v, want ErrSnapshotNotFound", err)
		}
	})

	t.Run("RoomsAreIsolated", func(t *testing.T) {
		s := factory()
		idA, err := s.SaveSnapshot(ctx, "roomA", "a", []byte("aaa"))
		if err != nil {
			t.Fatalf("SaveSnapshot(roomA): %v", err)
		}
		idB, err := s.SaveSnapshot(ctx, "roomB", "b", []byte("bbb"))
		if err != nil {
			t.Fatalf("SaveSnapshot(roomB): %v", err)
		}
		// Listings must not bleed across rooms.
		a, err := s.ListSnapshots(ctx, "roomA")
		if err != nil {
			t.Fatalf("ListSnapshots(roomA): %v", err)
		}
		if len(a) != 1 || a[0].Label != "a" {
			t.Fatalf("roomA = %+v, want single label a", a)
		}
		b, err := s.ListSnapshots(ctx, "roomB")
		if err != nil {
			t.Fatalf("ListSnapshots(roomB): %v", err)
		}
		if len(b) != 1 || b[0].Label != "b" {
			t.Fatalf("roomB = %+v, want single label b", b)
		}
		// State is keyed by (room, id): each room reads its own blob. IDs are only
		// unique within a room, so ids may legitimately collide across rooms.
		sa, err := s.GetSnapshotState(ctx, "roomA", idA)
		if err != nil {
			t.Fatalf("GetSnapshotState(roomA): %v", err)
		}
		sb, err := s.GetSnapshotState(ctx, "roomB", idB)
		if err != nil {
			t.Fatalf("GetSnapshotState(roomB): %v", err)
		}
		if !bytes.Equal(sa, []byte("aaa")) || !bytes.Equal(sb, []byte("bbb")) {
			t.Fatalf("states = %q / %q, want aaa / bbb", sa, sb)
		}
		// Deleting in one room must not affect the other.
		if err := s.DeleteSnapshot(ctx, "roomB", idB); err != nil {
			t.Fatalf("DeleteSnapshot(roomB): %v", err)
		}
		if _, err := s.GetSnapshotState(ctx, "roomA", idA); err != nil {
			t.Fatalf("roomA snapshot must survive roomB delete: %v", err)
		}
	})
}
