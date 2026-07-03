package crdt

import (
	"reflect"
	"testing"
)

func attrs(t *testing.T, pairs ...any) []*ContentAttribute {
	t.Helper()
	out := make([]*ContentAttribute, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		a, err := NewContentAttribute(pairs[i].(string), pairs[i+1])
		if err != nil {
			t.Fatalf("NewContentAttribute: %v", err)
		}
		out = append(out, a)
	}
	return out
}

func TestNewContentAttribute_Validation(t *testing.T) {
	for _, ok := range []any{nil, true, 42, int64(7), 3.14, "s", []byte{1}, []any{1, "x"}, map[string]any{"k": 1}} {
		if _, err := NewContentAttribute("n", ok); err != nil {
			t.Errorf("value %T should be valid: %v", ok, err)
		}
	}
	type notSupported struct{}
	for _, bad := range []any{notSupported{}, make(chan int), []any{notSupported{}}, map[string]any{"k": notSupported{}}} {
		if _, err := NewContentAttribute("n", bad); err == nil {
			t.Errorf("value %T should be rejected", bad)
		}
	}
}

// #56 overlap semantics (yjs AttrRanges.getIds): overlapping adds split ranges
// and union attrs; adjacent equal-attr ranges merge.
func TestIDMap_OverlapSplitAndJoin(t *testing.T) {
	m := NewIDMap()
	alice := attrs(t, "user", "alice")
	bob := attrs(t, "user", "bob")

	m.Add(1, 0, 10, alice) // [0,10) alice
	m.Add(1, 5, 10, bob)   // [5,15) bob → overlap [5,10) has both

	got := m.Ranges(1)
	if len(got) != 3 {
		t.Fatalf("want 3 ranges (split), got %d: %+v", len(got), got)
	}
	// [0,5) alice
	if got[0].Clock != 0 || got[0].Len != 5 || len(got[0].Attrs) != 1 || got[0].Attrs[0].Value != "alice" {
		t.Fatalf("range 0 wrong: %+v", got[0])
	}
	// [5,10) alice+bob (order: left attrs then joined right)
	if got[1].Clock != 5 || got[1].Len != 5 || len(got[1].Attrs) != 2 {
		t.Fatalf("range 1 wrong: %+v", got[1])
	}
	// [10,15) bob
	if got[2].Clock != 10 || got[2].Len != 5 || len(got[2].Attrs) != 1 || got[2].Attrs[0].Value != "bob" {
		t.Fatalf("range 2 wrong: %+v", got[2])
	}
}

// Same attr value added via distinct instances must intern to one instance,
// and adjacent equal-attr ranges must merge.
func TestIDMap_InternAndAdjacentMerge(t *testing.T) {
	m := NewIDMap()
	m.Add(1, 0, 5, attrs(t, "user", "alice"))
	m.Add(1, 5, 5, attrs(t, "user", "alice")) // distinct instance, equal value

	got := m.Ranges(1)
	if len(got) != 1 || got[0].Clock != 0 || got[0].Len != 10 {
		t.Fatalf("adjacent equal-attr ranges should merge: %+v", got)
	}
}

func TestIDMap_Slice(t *testing.T) {
	m := NewIDMap()
	a := attrs(t, "u", "a")
	m.Add(1, 5, 5, a) // [5,10)

	got := m.Slice(1, 0, 20)
	// Expect: gap [0,5), attributed [5,10), gap [10,20).
	if len(got) != 3 {
		t.Fatalf("want 3 slices, got %d: %+v", len(got), got)
	}
	if got[0].Attrs != nil || got[0].Clock != 0 || got[0].Len != 5 {
		t.Fatalf("slice 0 wrong: %+v", got[0])
	}
	if got[1].Attrs == nil || got[1].Clock != 5 || got[1].Len != 5 {
		t.Fatalf("slice 1 wrong: %+v", got[1])
	}
	if got[2].Attrs != nil || got[2].Clock != 10 || got[2].Len != 10 {
		t.Fatalf("slice 2 wrong: %+v", got[2])
	}
	// Fully unattributed span → single nil-attrs range.
	whole := m.Slice(2, 0, 4)
	if !reflect.DeepEqual(whole, []AttrRange{{Clock: 0, Len: 4, Attrs: nil}}) {
		t.Fatalf("unknown-client slice wrong: %+v", whole)
	}
}
