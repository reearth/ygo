package crdt

import (
	"reflect"
	"testing"
)

// TestApplyDelta_ConsecutiveInsertsStayInOrder pins the fix for a cursor bug
// in the ApplyDelta single-threaded-cursor rewrite (#181, commit 481c949):
// applyDeltaInsert only advanced pos.left/pos.right past the just-inserted
// item when that insert carried its own Attributes (i.e. the diff/open-close
// marker path ran). A plain attribute-less insert left the cursor stale at
// its PRE-insert position, so the next op (whatever it was) re-anchored at
// the same left neighbour and could integrate out of order.
//
// This was flagged (but deliberately not exercised) by
// TestApplyDelta_InsertDoesNotInheritPrecedingRetainFormat's doc comment,
// which sidesteps it with an intervening Retain. This test exercises it
// directly: two attribute-less inserts back-to-back on an empty document.
func TestApplyDelta_ConsecutiveInsertsStayInOrder(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) {
		txt.ApplyDelta(tr, []Delta{
			{Op: DeltaOpInsert, Insert: "a"},
			{Op: DeltaOpInsert, Insert: "b"},
		})
	})

	if got := txt.ToString(); got != "ab" {
		t.Fatalf("ToString() = %q, want %q", got, "ab")
	}

	want := []Delta{{Op: DeltaOpInsert, Insert: "ab"}}
	if got := txt.ToDelta(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ToDelta() = %#v, want %#v", got, want)
	}
}

// TestApplyDelta_PlainInsertThenAttributedInsertStaysInOrder covers a plain
// (attribute-less) insert immediately followed by an insert that DOES carry
// its own attributes. Before the fix, the cursor left stale by the first
// insert would cause the second insert to anchor at the same left neighbour
// as the first, instead of after it.
func TestApplyDelta_PlainInsertThenAttributedInsertStaysInOrder(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) {
		txt.ApplyDelta(tr, []Delta{
			{Op: DeltaOpInsert, Insert: "a"},
			{Op: DeltaOpInsert, Insert: "b", Attributes: Attributes{"bold": true}},
		})
	})

	if got := txt.ToString(); got != "ab" {
		t.Fatalf("ToString() = %q, want %q", got, "ab")
	}

	want := []Delta{
		{Op: DeltaOpInsert, Insert: "a"},
		{Op: DeltaOpInsert, Insert: "b", Attributes: Attributes{"bold": true}},
	}
	if got := txt.ToDelta(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ToDelta() = %#v, want %#v", got, want)
	}
}

// TestApplyDelta_PlainInsertThenRetainFormatStaysInOrder covers a plain
// insert immediately followed by a {retain, attributes} op. Before the fix,
// the retain-with-attributes op (applyFormatAtPos) would resolve its range
// starting from the stale pos.left/pos.right, formatting the wrong span
// (potentially re-covering the just-inserted item, or the wrong following
// content) instead of the content that actually follows the insert.
func TestApplyDelta_PlainInsertThenRetainFormatStaysInOrder(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) {
		txt.Insert(tr, 0, "xyz", nil)
		txt.ApplyDelta(tr, []Delta{
			{Op: DeltaOpInsert, Insert: "a"},
			{Op: DeltaOpRetain, Retain: 1, Attributes: Attributes{"bold": true}},
		})
	})

	if got := txt.ToString(); got != "axyz" {
		t.Fatalf("ToString() = %q, want %q", got, "axyz")
	}

	want := []Delta{
		{Op: DeltaOpInsert, Insert: "a"},
		{Op: DeltaOpInsert, Insert: "x", Attributes: Attributes{"bold": true}},
		{Op: DeltaOpInsert, Insert: "yz"},
	}
	if got := txt.ToDelta(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ToDelta() = %#v, want %#v", got, want)
	}
}
