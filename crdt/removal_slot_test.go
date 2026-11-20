package crdt

import "testing"

// Removals from a list a type or an attribution map keeps clear the slot they
// vacate. What these lists hold stays reachable otherwise - a marker's item
// sits in the store, an attribution's attributes in the merged range - so the
// tests read the vacated slot itself.

// requireVacatedClear fails when a slot of s past its length, up to its
// capacity, holds anything but the zero value.
func requireVacatedClear[T comparable](t *testing.T, what string, s []T) {
	t.Helper()
	var zero T
	for i, v := range s[len(s):cap(s)] {
		if v != zero {
			t.Fatalf("%s: the slot %d past the length still holds %+v", what, len(s)+i, v)
		}
	}
}

// A marker on a deleted item (updateMarkerChanges) and one on an uncountable
// item at the index (dropUncountedMarkersAt) are dropped with their slot
// cleared.
func TestUnit_SearchMarkers_ADroppedMarkerLeavesNoSlotBehind(t *testing.T) {
	kept := &Item{Content: NewContentString("a")}
	deleted := &Item{Content: NewContentString("b"), Deleted: true}
	typ := &abstractType{markers: []searchMarker{{item: kept, index: 0}, {item: deleted, index: 1}}}
	typ.updateMarkerChanges(0, 0)
	if len(typ.markers) != 1 {
		t.Fatalf("%d markers kept, want the one on the live item", len(typ.markers))
	}
	requireVacatedClear(t, "updateMarkerChanges", typ.markers)

	uncounted := &Item{Content: NewContentDeleted(1)}
	typ = &abstractType{markers: []searchMarker{{item: kept, index: 0}, {item: uncounted, index: 3}}}
	typ.dropUncountedMarkersAt(3)
	if len(typ.markers) != 1 {
		t.Fatalf("%d markers kept, want the one on the countable item", len(typ.markers))
	}
	requireVacatedClear(t, "dropUncountedMarkersAt", typ.markers)
}

// Two attributed ranges over the same clocks merge into one, and the range
// merged away leaves no slot behind in the list the map keeps (getIDs).
func TestUnit_AttrRanges_AMergedRangeLeavesNoSlotBehind(t *testing.T) {
	r := &attrRanges{ids: []AttrRange{
		{Clock: 0, Len: 2, Attrs: []*ContentAttribute{{Name: "a", Value: 1}}},
		{Clock: 0, Len: 2, Attrs: []*ContentAttribute{{Name: "b", Value: 2}}},
	}}
	if ids := r.getIDs(); len(ids) != 1 {
		t.Fatalf("%d ranges, want the two merged into one", len(ids))
	}
	for i, v := range r.ids[len(r.ids):cap(r.ids)] {
		if v.Attrs != nil || v.Len != 0 {
			t.Fatalf("getIDs: the slot %d past the length still holds %+v", len(r.ids)+i, v)
		}
	}
}

// A format that changes nothing splits a struct at its edges and inserts
// nothing between the halves, so the commit merges them back, and each half
// merged away leaves no slot behind in the client's store list
// (tryMergeWithLeft).
func TestUnit_StructStore_AMergedStructLeavesNoSlotBehind(t *testing.T) {
	doc := New(WithClientID(1))
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, "abc", nil) })
	doc.Transact(func(txn *Transaction) { text.Format(txn, 1, 1, Attributes{"bold": nil}) })
	items := doc.store.clients[1]
	if len(items) != 1 {
		t.Fatalf("%d structs of client 1, want the split halves merged back into one", len(items))
	}
	requireVacatedClear(t, "tryMergeWithLeft", items)
}
