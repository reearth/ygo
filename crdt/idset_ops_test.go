package crdt

import (
	"reflect"
	"testing"
)

func idsetOf(t *testing.T, client ClientID, ranges ...IDRange) *IDSet {
	t.Helper()
	s := NewIDSet()
	for _, r := range ranges {
		s.Add(client, r.Clock, r.Len)
	}
	return s
}

func TestMergeIDSets(t *testing.T) {
	a := idsetOf(t, 1, IDRange{0, 5})
	b := idsetOf(t, 1, IDRange{3, 4}) // overlaps a
	b.Add(2, 10, 2)
	m := MergeIDSets(a, b)
	if got, want := m.Ranges(1), []IDRange{{0, 7}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged client1 = %v, want %v", got, want)
	}
	if got, want := m.Ranges(2), []IDRange{{10, 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged client2 = %v, want %v", got, want)
	}
	// Inputs untouched.
	if !reflect.DeepEqual(a.Ranges(1), []IDRange{{0, 5}}) {
		t.Fatal("MergeIDSets must not mutate inputs")
	}
}

// Port-fidelity cases for the _diffSet two-pointer walk.
func TestExcludeIDSet(t *testing.T) {
	cases := []struct {
		name         string
		set, exclude []IDRange
		want         []IDRange
	}{
		{"no overlap", []IDRange{{0, 5}}, []IDRange{{10, 5}}, []IDRange{{0, 5}}},
		{"exclude head", []IDRange{{0, 10}}, []IDRange{{0, 4}}, []IDRange{{4, 6}}},
		{"exclude tail", []IDRange{{0, 10}}, []IDRange{{6, 10}}, []IDRange{{0, 6}}},
		{"exclude middle splits", []IDRange{{0, 10}}, []IDRange{{3, 4}}, []IDRange{{0, 3}, {7, 3}}},
		{"exclude covers all", []IDRange{{2, 4}}, []IDRange{{0, 10}}, nil},
		{"multi vs multi", []IDRange{{0, 4}, {8, 4}}, []IDRange{{2, 8}}, []IDRange{{0, 2}, {10, 2}}},
	}
	for _, tc := range cases {
		set := idsetOf(t, 1, tc.set...)
		excl := idsetOf(t, 1, tc.exclude...)
		got := ExcludeIDSet(set, excl).Ranges(1)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIntersectIDSets(t *testing.T) {
	a := idsetOf(t, 1, IDRange{0, 10}, IDRange{20, 5})
	b := idsetOf(t, 1, IDRange{5, 20})
	got := IntersectIDSets(a, b).Ranges(1)
	want := []IDRange{{5, 5}, {20, 5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("intersect = %v, want %v", got, want)
	}
	if !IntersectIDSets(a, NewIDSet()).IsEmpty() {
		t.Fatal("intersect with empty must be empty")
	}
}

func TestIDSetFromIDMap(t *testing.T) {
	m := NewIDMap()
	m.Add(1, 0, 5, attrs(t, "u", "a"))
	m.Add(1, 5, 5, attrs(t, "u", "b")) // different attrs, adjacent → one IDSet range
	got := IDSetFromIDMap(m).Ranges(1)
	if !reflect.DeepEqual(got, []IDRange{{0, 10}}) {
		t.Fatalf("IDSetFromIDMap = %v, want [{0 10}]", got)
	}
}

// --- Nil-half tolerance (issue #56 final review: nil-half dereference panic) ---

func TestExcludeIDSet_Nil(t *testing.T) {
	s := idsetOf(t, 1, IDRange{0, 5})
	if got := ExcludeIDSet(nil, s); !got.IsEmpty() {
		t.Fatalf("ExcludeIDSet(nil, s) = %v, want empty", got)
	}
	if got, want := ExcludeIDSet(s, nil).Ranges(1), s.Ranges(1); !reflect.DeepEqual(got, want) {
		t.Fatalf("ExcludeIDSet(s, nil) = %v, want copy of s %v", got, want)
	}
	if got := ExcludeIDSet(nil, nil); !got.IsEmpty() {
		t.Fatalf("ExcludeIDSet(nil, nil) = %v, want empty", got)
	}
}

func TestIntersectIDSets_Nil(t *testing.T) {
	s := idsetOf(t, 1, IDRange{0, 5})
	if got := IntersectIDSets(nil, s); !got.IsEmpty() {
		t.Fatalf("IntersectIDSets(nil, s) = %v, want empty", got)
	}
	if got := IntersectIDSets(s, nil); !got.IsEmpty() {
		t.Fatalf("IntersectIDSets(s, nil) = %v, want empty", got)
	}
	if got := IntersectIDSets(nil, nil); !got.IsEmpty() {
		t.Fatalf("IntersectIDSets(nil, nil) = %v, want empty", got)
	}
}

func TestIDSetFromIDMap_Nil(t *testing.T) {
	if got := IDSetFromIDMap(nil); !got.IsEmpty() {
		t.Fatalf("IDSetFromIDMap(nil) = %v, want empty", got)
	}
}
