package crdt

import (
	"reflect"
	"testing"
)

// The stamping flow of issue #56: update → ContentIDs → stamp → encode → decode.
func TestContentIDsFromUpdateV1_AndStamp(t *testing.T) {
	src := New(WithClientID(7))
	txt := src.GetText("t")
	src.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	src.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 2) }) // delete "he"
	update := EncodeStateAsUpdateV1(src, nil)

	ids, err := ContentIDsFromUpdateV1(update)
	if err != nil {
		t.Fatalf("ContentIDsFromUpdateV1: %v", err)
	}
	// All 5 inserted clocks are present as inserts.
	for clock := uint64(0); clock < 5; clock++ {
		if !ids.Inserts.Has(7, clock) {
			t.Fatalf("insert clock %d missing", clock)
		}
	}
	// The two deleted clocks are in deletes.
	if !ids.Deletes.Has(7, 0) || !ids.Deletes.Has(7, 1) || ids.Deletes.Has(7, 2) {
		t.Fatalf("deletes wrong: %+v", ids.Deletes.Ranges(7))
	}

	cm := CreateContentMapFromContentIDs(ids, attrs(t, "userid", "alice", "ts", int64(1000)), nil)
	if cm.Inserts.IsEmpty() || cm.Deletes.IsEmpty() {
		t.Fatal("stamped content map should have both halves")
	}
	// deleteAttrs defaulted to insertAttrs.
	if got := cm.Deletes.Ranges(7); len(got[0].Attrs) != 2 {
		t.Fatalf("delete attrs not defaulted: %+v", got)
	}

	// Codec round-trip.
	decoded, err := DecodeContentMap(EncodeContentMap(cm))
	if err != nil {
		t.Fatalf("DecodeContentMap: %v", err)
	}
	if !reflect.DeepEqual(decoded.Inserts.Clients(), cm.Inserts.Clients()) {
		t.Fatal("content map round-trip lost insert clients")
	}
	idsRT, err := DecodeContentIDs(EncodeContentIDs(ids))
	if err != nil {
		t.Fatalf("DecodeContentIDs: %v", err)
	}
	if !reflect.DeepEqual(idsRT.Inserts.Ranges(7), ids.Inserts.Ranges(7)) {
		t.Fatal("content ids round-trip lost inserts")
	}
}

func TestInsertAndDeleteSetFromDoc(t *testing.T) {
	doc := New(WithClientID(3))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcd", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 1, 2) })

	all := InsertSetFromDoc(doc, false)
	if !all.Has(3, 0) || !all.Has(3, 3) {
		t.Fatalf("full insert set wrong: %+v", all.Ranges(3))
	}
	live := InsertSetFromDoc(doc, true) // filterDeleted: deleted runs excluded
	if !live.Has(3, 0) || live.Has(3, 1) || live.Has(3, 2) || !live.Has(3, 3) {
		t.Fatalf("filtered insert set wrong: %+v", live.Ranges(3))
	}
	dels := DeleteSetFromDoc(doc)
	if !dels.Has(3, 1) || !dels.Has(3, 2) || dels.Has(3, 0) {
		t.Fatalf("delete set wrong: %+v", dels.Ranges(3))
	}
}

func TestContentMapAlgebraWrappers(t *testing.T) {
	mk := func(clock uint64, user string) ContentMap {
		m := NewIDMap()
		m.Add(1, clock, 5, attrs(t, "u", user))
		return ContentMap{Inserts: m, Deletes: NewIDMap()}
	}
	a, b := mk(0, "a"), mk(5, "b")
	merged := MergeContentMaps(a, b)
	if got := merged.Inserts.Ranges(1); len(got) != 2 {
		t.Fatalf("merge wrapper wrong: %+v", got)
	}
	excluded := ExcludeContentMap(merged, ContentIDs{Inserts: idsetOf(t, 1, IDRange{0, 5}), Deletes: NewIDSet()})
	if got := excluded.Inserts.Ranges(1); len(got) != 1 || got[0].Clock != 5 {
		t.Fatalf("exclude wrapper wrong: %+v", got)
	}
	filtered := FilterContentMap(merged,
		func(as []*ContentAttribute) bool { return as[0].Value == "a" },
		func([]*ContentAttribute) bool { return true })
	if got := filtered.Inserts.Ranges(1); len(got) != 1 || got[0].Clock != 0 {
		t.Fatalf("filter wrapper wrong: %+v", got)
	}
	inter := IntersectContentMaps(merged, a)
	if got := inter.Inserts.Ranges(1); len(got) != 1 || got[0].Clock != 0 || got[0].Len != 5 {
		t.Fatalf("intersect wrapper wrong: %+v", got)
	}
}

func TestContentIDsFromUpdateV1_Malformed(t *testing.T) {
	if _, err := ContentIDsFromUpdateV1([]byte{0xFF, 0x01, 0x02}); err == nil {
		t.Fatal("malformed update should error")
	}
}

// --- Nil-half tolerance (issue #56 final review: nil-half dereference panic) ---

func TestContentMapAlgebraWrappers_NilHalves(t *testing.T) {
	keepAll := func([]*ContentAttribute) bool { return true }

	excluded := ExcludeContentMap(ContentMap{}, ContentIDs{})
	if !excluded.Inserts.IsEmpty() || !excluded.Deletes.IsEmpty() {
		t.Fatalf("ExcludeContentMap(ContentMap{}, ContentIDs{}) = %+v, want empty halves", excluded)
	}

	intersected := IntersectContentMaps(ContentMap{}, ContentMap{})
	if !intersected.Inserts.IsEmpty() || !intersected.Deletes.IsEmpty() {
		t.Fatalf("IntersectContentMaps(ContentMap{}, ContentMap{}) = %+v, want empty halves", intersected)
	}

	filtered := FilterContentMap(ContentMap{}, keepAll, keepAll)
	if !filtered.Inserts.IsEmpty() || !filtered.Deletes.IsEmpty() {
		t.Fatalf("FilterContentMap(ContentMap{}, keepAll, keepAll) = %+v, want empty halves", filtered)
	}
}
