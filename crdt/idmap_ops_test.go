package crdt

import (
	"reflect"
	"testing"
)

func TestMergeIDMaps_RemapsInterning(t *testing.T) {
	a := NewIDMap()
	a.Add(1, 0, 5, attrs(t, "u", "alice"))
	b := NewIDMap()
	b.Add(1, 5, 5, attrs(t, "u", "alice")) // equal value, distinct instance
	m := MergeIDMaps(a, b)
	got := m.Ranges(1)
	// Equal attrs re-interned to one instance → adjacent ranges merge.
	if len(got) != 1 || got[0].Clock != 0 || got[0].Len != 10 {
		t.Fatalf("merge should re-intern and coalesce: %+v", got)
	}
}

func TestExcludeIDMap(t *testing.T) {
	m := NewIDMap()
	m.Add(1, 0, 10, attrs(t, "u", "a"))
	got := ExcludeIDMap(m, idsetOf(t, 1, IDRange{3, 4})).Ranges(1)
	if len(got) != 2 || got[0].Clock != 0 || got[0].Len != 3 || got[1].Clock != 7 || got[1].Len != 3 {
		t.Fatalf("exclude split wrong: %+v", got)
	}
	if got[0].Attrs[0].Value != "a" || got[1].Attrs[0].Value != "a" {
		t.Fatal("exclude must preserve attrs")
	}
}

func TestIntersectIDMaps_ConcatsAttrs(t *testing.T) {
	a := NewIDMap()
	a.Add(1, 0, 10, attrs(t, "u", "a"))
	b := NewIDMap()
	b.Add(1, 5, 10, attrs(t, "r", true))
	got := IntersectIDMaps(a, b).Ranges(1)
	if len(got) != 1 || got[0].Clock != 5 || got[0].Len != 5 || len(got[0].Attrs) != 2 {
		t.Fatalf("intersect wrong: %+v", got)
	}
}

func TestFilterIDMap(t *testing.T) {
	m := NewIDMap()
	m.Add(1, 0, 5, attrs(t, "u", "a"))
	m.Add(1, 10, 5, attrs(t, "u", "b"))
	got := FilterIDMap(m, func(as []*ContentAttribute) bool { return as[0].Value == "b" }).Ranges(1)
	if len(got) != 1 || got[0].Clock != 10 {
		t.Fatalf("filter wrong: %+v", got)
	}
}

func TestIDMapFromIDSet(t *testing.T) {
	s := idsetOf(t, 1, IDRange{0, 5}, IDRange{10, 2})
	m := IDMapFromIDSet(s, attrs(t, "userid", "alice", "ts", int64(99)))
	got := m.Ranges(1)
	if len(got) != 2 || len(got[0].Attrs) != 2 || len(got[1].Attrs) != 2 {
		t.Fatalf("stamping wrong: %+v", got)
	}
	// Stamped instances are interned: both ranges share the same pointers.
	if got[0].Attrs[0] != got[1].Attrs[0] {
		t.Fatal("stamped attrs should be shared interned instances")
	}
	if !reflect.DeepEqual(IDSetFromIDMap(m).Ranges(1), s.Ranges(1)) {
		t.Fatal("projection back to IDSet should match source")
	}
}

// --- Nil-half tolerance (issue #56 final review: nil-half dereference panic) ---

func TestExcludeIDMap_Nil(t *testing.T) {
	s := idsetOf(t, 1, IDRange{0, 5})
	m := NewIDMap()
	m.Add(1, 0, 5, attrs(t, "u", "a"))

	if got := ExcludeIDMap(nil, s); !got.IsEmpty() {
		t.Fatalf("ExcludeIDMap(nil, s) = %v, want empty", got)
	}
	if got, want := ExcludeIDMap(m, nil).Ranges(1), m.Ranges(1); !reflect.DeepEqual(got, want) {
		t.Fatalf("ExcludeIDMap(m, nil) = %+v, want copy of m %+v", got, want)
	}
	if got := ExcludeIDMap(nil, nil); !got.IsEmpty() {
		t.Fatalf("ExcludeIDMap(nil, nil) = %v, want empty", got)
	}
}

func TestIntersectIDMaps_Nil(t *testing.T) {
	m := NewIDMap()
	m.Add(1, 0, 5, attrs(t, "u", "a"))

	if got := IntersectIDMaps(nil, m); !got.IsEmpty() {
		t.Fatalf("IntersectIDMaps(nil, m) = %v, want empty", got)
	}
	if got := IntersectIDMaps(m, nil); !got.IsEmpty() {
		t.Fatalf("IntersectIDMaps(m, nil) = %v, want empty", got)
	}
	if got := IntersectIDMaps(nil, nil); !got.IsEmpty() {
		t.Fatalf("IntersectIDMaps(nil, nil) = %v, want empty", got)
	}
}

func TestFilterIDMap_Nil(t *testing.T) {
	pred := func([]*ContentAttribute) bool { return true }
	if got := FilterIDMap(nil, pred); !got.IsEmpty() {
		t.Fatalf("FilterIDMap(nil, pred) = %v, want empty", got)
	}
}

func TestIDMapFromIDSet_Nil(t *testing.T) {
	if got := IDMapFromIDSet(nil, attrs(t, "u", "a")); !got.IsEmpty() {
		t.Fatalf("IDMapFromIDSet(nil, attrs) = %v, want empty", got)
	}
}
