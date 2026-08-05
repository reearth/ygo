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

	// Parameterised over conformanceRoomNames: the room name reaches the
	// snapshot path's name-derived surface (the per-room counter object embeds
	// it, and IDs are often recovered by parsing them back out of an object
	// name), so an awkward name can corrupt ID handling rather than fail
	// loudly. See conformance_names.go and issue #211.
	t.Run("SaveThenListNewestFirst", func(t *testing.T) {
		for _, room := range conformanceRoomNames {
			t.Run(room, func(t *testing.T) {
				s := factory()
				id1, err := s.SaveSnapshot(ctx, room, "first", []byte("state-1"))
				if err != nil {
					t.Fatalf("SaveSnapshot(first): %v", err)
				}
				id2, err := s.SaveSnapshot(ctx, room, "second", []byte("state-22"))
				if err != nil {
					t.Fatalf("SaveSnapshot(second): %v", err)
				}
				if id1 == id2 {
					t.Fatalf("ids must be distinct, both = %d", id1)
				}
				if id2 <= id1 {
					t.Fatalf("ids must increase: id1=%d id2=%d", id1, id2)
				}

				got, err := s.ListSnapshots(ctx, room)
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
		for _, room := range conformanceRoomNames {
			t.Run(room, func(t *testing.T) {
				s := factory()
				want := []byte("the-state-blob")
				id, err := s.SaveSnapshot(ctx, room, "lbl", want)
				if err != nil {
					t.Fatalf("SaveSnapshot: %v", err)
				}
				got, err := s.GetSnapshotState(ctx, room, id)
				if err != nil {
					t.Fatalf("GetSnapshotState: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("state = %q, want %q", got, want)
				}
			})
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

	// Size must be the state length exactly, with neither a record header nor the
	// label counted in. A backend that derives Size from a file size (the file
	// backend does, to keep listing cheap) gets its arithmetic checked here, with
	// a deliberately non-round length and both a long and an empty label.
	t.Run("SizeIsExactRegardlessOfLabel", func(t *testing.T) {
		s := factory()
		big := bytes.Repeat([]byte("q"), 64*1024+7)

		labelled, err := s.SaveSnapshot(ctx, "room", "a-deliberately-long-label", big)
		if err != nil {
			t.Fatalf("SaveSnapshot(labelled): %v", err)
		}
		unlabelled, err := s.SaveSnapshot(ctx, "room", "", big)
		if err != nil {
			t.Fatalf("SaveSnapshot(unlabelled): %v", err)
		}

		got, err := s.ListSnapshots(ctx, "room")
		if err != nil {
			t.Fatalf("ListSnapshots: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		for _, info := range got {
			if info.Size != int64(len(big)) {
				t.Fatalf("id %d (label %q) Size = %d, want %d", info.ID, info.Label, info.Size, len(big))
			}
		}

		// The payload itself must still round-trip byte-for-byte.
		for _, id := range []int64{labelled, unlabelled} {
			state, err := s.GetSnapshotState(ctx, "room", id)
			if err != nil {
				t.Fatalf("GetSnapshotState(%d): %v", id, err)
			}
			if !bytes.Equal(state, big) {
				t.Fatalf("id %d state differs: got %d bytes, want %d", id, len(state), len(big))
			}
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
		for _, room := range conformanceRoomNames {
			t.Run(room, func(t *testing.T) {
				s := factory()
				keep, err := s.SaveSnapshot(ctx, room, "keep", []byte("k"))
				if err != nil {
					t.Fatalf("SaveSnapshot(keep): %v", err)
				}
				drop, err := s.SaveSnapshot(ctx, room, "drop", []byte("d"))
				if err != nil {
					t.Fatalf("SaveSnapshot(drop): %v", err)
				}
				if err := s.DeleteSnapshot(ctx, room, drop); err != nil {
					t.Fatalf("DeleteSnapshot: %v", err)
				}
				got, err := s.ListSnapshots(ctx, room)
				if err != nil {
					t.Fatalf("ListSnapshots: %v", err)
				}
				if len(got) != 1 || got[0].ID != keep {
					t.Fatalf("after delete got %+v, want only id=%d", got, keep)
				}
				if _, err := s.GetSnapshotState(ctx, room, drop); !errors.Is(err, ErrSnapshotNotFound) {
					t.Fatalf("deleted snapshot err = %v, want ErrSnapshotNotFound", err)
				}
				if _, err := s.GetSnapshotState(ctx, room, keep); err != nil {
					t.Fatalf("kept snapshot must survive: %v", err)
				}
			})
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

	// Parameterised over pairs of DISTINCT rooms whose names collide under a
	// naive encoding. This is the case that catches the sharp edge in #211:
	// snapshot IDs are often recovered by parsing them back out of an object
	// name, so two rooms collapsing to one key silently maps one room's IDs
	// onto the other's instead of failing, and a wrong ID decides which state a
	// user restores.
	t.Run("RoomsAreIsolated", func(t *testing.T) {
		pairs := []struct {
			name string
			a, b string
		}{
			// Both separators mapped to the same replacement character.
			{"separator", "a/b", "a:b"},
			// Percent-encoding applied to one name but not the other.
			{"percent", "a/b", "a%2Fb"},
			// Space encoded as "+".
			{"space-plus", "a b", "a+b"},
			// NFC vs NFD: distinct bytes that collide if the backend lets a
			// normalizing filesystem name the object. Escaped deliberately —
			// see conformance_names.go.
			{"unicode-normalization", "\u00fc\u00f1\u00ef", "u\u0308n\u0303i\u0308"},
		}
		for _, p := range pairs {
			t.Run(p.name, func(t *testing.T) {
				// The pair must be two DIFFERENT rooms or this case asserts
				// nothing. Cheap insurance: the normalization pair in
				// particular is two visually identical literals, and an editor
				// or tool that normalizes the file would collapse them into one
				// string, leaving a test that passes while checking nothing.
				if p.a == p.b {
					t.Fatalf("pair %q is the same room twice (%q) — the names were normalized away", p.name, p.a)
				}
				s := factory()
				idA, err := s.SaveSnapshot(ctx, p.a, "a", []byte("aaa"))
				if err != nil {
					t.Fatalf("SaveSnapshot(%q): %v", p.a, err)
				}
				idB, err := s.SaveSnapshot(ctx, p.b, "b", []byte("bbb"))
				if err != nil {
					t.Fatalf("SaveSnapshot(%q): %v", p.b, err)
				}
				// Listings must not bleed across rooms.
				a, err := s.ListSnapshots(ctx, p.a)
				if err != nil {
					t.Fatalf("ListSnapshots(%q): %v", p.a, err)
				}
				if len(a) != 1 || a[0].Label != "a" {
					t.Fatalf("room %q = %+v, want single label a", p.a, a)
				}
				b, err := s.ListSnapshots(ctx, p.b)
				if err != nil {
					t.Fatalf("ListSnapshots(%q): %v", p.b, err)
				}
				if len(b) != 1 || b[0].Label != "b" {
					t.Fatalf("room %q = %+v, want single label b", p.b, b)
				}
				// State is keyed by (room, id): each room reads its own blob. IDs
				// are only unique within a room, so ids may legitimately collide
				// across rooms — do NOT assert the other room's id is missing,
				// that would fail on a correct backend.
				sa, err := s.GetSnapshotState(ctx, p.a, idA)
				if err != nil {
					t.Fatalf("GetSnapshotState(%q): %v", p.a, err)
				}
				sb, err := s.GetSnapshotState(ctx, p.b, idB)
				if err != nil {
					t.Fatalf("GetSnapshotState(%q): %v", p.b, err)
				}
				if !bytes.Equal(sa, []byte("aaa")) || !bytes.Equal(sb, []byte("bbb")) {
					t.Fatalf("states = %q / %q, want aaa / bbb", sa, sb)
				}
				// Deleting in one room must not affect the other.
				if err := s.DeleteSnapshot(ctx, p.b, idB); err != nil {
					t.Fatalf("DeleteSnapshot(%q): %v", p.b, err)
				}
				if _, err := s.GetSnapshotState(ctx, p.a, idA); err != nil {
					t.Fatalf("room %q snapshot must survive %q delete: %v", p.a, p.b, err)
				}
			})
		}
	})
}
