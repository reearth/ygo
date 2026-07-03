package crdt

import (
	"reflect"
	"testing"
)

func TestIDSet_AddNormalizeHas(t *testing.T) {
	s := NewIDSet()
	if !s.IsEmpty() {
		t.Fatal("new IDSet should be empty")
	}
	// Out-of-order, overlapping, adjacent, and duplicate adds for one client.
	s.Add(1, 10, 5) // [10,15)
	s.Add(1, 0, 3)  // [0,3)
	s.Add(1, 14, 4) // overlaps [10,15) → merge to [10,18)
	s.Add(1, 3, 2)  // adjacent to [0,3) → merge to [0,5)
	s.Add(1, 30, 0) // zero-length: no-op
	s.Add(2, 7, 1)

	want1 := []IDRange{{Clock: 0, Len: 5}, {Clock: 10, Len: 8}}
	if got := s.Ranges(1); !reflect.DeepEqual(got, want1) {
		t.Fatalf("Ranges(1) = %v, want %v", got, want1)
	}
	if got := s.Clients(); !reflect.DeepEqual(got, []ClientID{1, 2}) {
		t.Fatalf("Clients() = %v, want [1 2]", got)
	}
	for _, tc := range []struct {
		clock uint64
		want  bool
	}{{0, true}, {4, true}, {5, false}, {9, false}, {10, true}, {17, true}, {18, false}} {
		if got := s.Has(1, tc.clock); got != tc.want {
			t.Errorf("Has(1, %d) = %v, want %v", tc.clock, got, tc.want)
		}
	}
	if s.HasID(ID{Client: 2, Clock: 7}) != true {
		t.Error("HasID(2,7) should be true")
	}
	if s.IsEmpty() {
		t.Error("IsEmpty after adds should be false")
	}

	// Clone is independent.
	c := s.Clone()
	c.Add(1, 100, 1)
	if s.Has(1, 100) {
		t.Error("mutating clone must not affect original")
	}
}

func TestIDSet_RangesUnknownClient(t *testing.T) {
	s := NewIDSet()
	if got := s.Ranges(99); got != nil {
		t.Fatalf("Ranges(unknown) = %v, want nil", got)
	}
}
