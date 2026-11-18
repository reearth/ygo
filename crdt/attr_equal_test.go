package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Yjs:
//
//	const equalAttrs = (a, b) =>
//	  a === b || (typeof a === 'object' && typeof b === 'object' && a && b &&
//	              object.equalFlat(a, b))
//
// Scalars follow === , so a number compares by value whatever its Go type and
// two objects with equal entries are equal. Yjs compares a NESTED object by
// identity; ygo compares it by content, because textValueAt copies every map
// and slice a caller passes, so no two values can share one. See attrEqual.
func TestUnit_AttrEqual_MatchesYjsEqualAttrs(t *testing.T) {
	nested := map[string]any{"y": 1}
	shared := map[string]any{"s": 1}

	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil vs value", nil, 1, false},

		{"same string", "x", "x", true},
		{"different string", "x", "y", false},
		{"same bool", true, true, true},
		{"bool vs string", true, "true", false},

		// === on numbers is by value: Go's integer and float types are one JS number.
		{"int64 vs float64, same value", int64(1), float64(1), true},
		{"int vs float64, same value", 1, float64(1), true},
		{"float32 vs float64, same value", float32(2), float64(2), true},
		{"different numbers", int64(1), int64(2), false},
		{"number vs string", int64(1), "1", false},

		// Flat object compare.
		{"empty slice vs empty map", []any{}, map[string]any{}, true},
		{"flat maps, equal contents", map[string]any{"x": 1}, map[string]any{"x": 1}, true},
		{"flat maps, different value", map[string]any{"x": 1}, map[string]any{"x": 2}, false},
		{"flat maps, different key", map[string]any{"x": 1}, map[string]any{"z": 1}, false},
		{"flat maps, different size", map[string]any{"x": 1}, map[string]any{"x": 1, "z": 2}, false},
		{"flat map values compare as numbers", map[string]any{"x": int64(1)}, map[string]any{"x": float64(1)}, true},

		{"slices, equal contents", []any{1, 2}, []any{1, 2}, true},
		{"slices, different contents", []any{1, 2}, []any{1, 3}, false},
		{"slices, different length", []any{1}, []any{1, 2}, false},
		{"byte slices, equal contents", []byte{1}, []byte{1}, true},
		{"byte slices, different contents", []byte{1}, []byte{2}, false},

		// Equal by index keys, as JS objects.
		{"slice vs map with index keys", []any{7}, map[string]any{"0": 7}, true},

		// Nested values compare by content: a caller's object never reaches the
		// document intact, so identity here would make every re-format of a
		// nested attribute write a marker.
		{"nested objects, equal contents", map[string]any{"x": map[string]any{"y": 1}}, map[string]any{"x": map[string]any{"y": 1}}, true},
		{"nested objects, different contents", map[string]any{"x": map[string]any{"y": 1}}, map[string]any{"x": map[string]any{"y": 2}}, false},
		{"nested object, same reference", map[string]any{"x": nested}, map[string]any{"x": nested}, true},
		{"nested numbers compare as numbers", map[string]any{"x": map[string]any{"y": int64(1)}}, map[string]any{"x": map[string]any{"y": float64(1)}}, true},
		{"same map reference", shared, shared, true},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, attrEqual(c.a, c.b), "attrEqual(%v, %v) [%s]", c.a, c.b, c.name)
		assert.Equal(t, c.want, attrEqual(c.b, c.a), "attrEqual is symmetric: %s", c.name)
	}
}

// Re-formatting with an unchanged nested attribute must stay a no-op. Yjs
// settles this with === on the caller's own object; ygo copies that object, so
// only a content compare keeps it a no-op. Comparing nested values by identity
// instead grew this document from 72 to 133 bytes and added a third marker.
func TestUnit_Format_UnchangedNestedAttributeWritesNoMarker(t *testing.T) {
	markers := func(txt *YText) int {
		n := 0
		for it := txt.start; it != nil; it = it.Right {
			if !it.Deleted && isContentFormat(it) {
				n++
			}
		}
		return n
	}
	attrs := func() Attributes {
		return Attributes{"meta": map[string]any{"style": map[string]any{"s": "bold"}}}
	}

	d := New()
	txt := d.GetText("t")
	d.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ab", attrs()) })
	wantMarkers, wantBytes := markers(txt), len(EncodeStateAsUpdateV1(d, nil))

	d.Transact(func(txn *Transaction) { txt.Format(txn, 0, 2, attrs()) })

	assert.Equal(t, wantMarkers, markers(txt), "a redundant marker was written")
	assert.Len(t, EncodeStateAsUpdateV1(d, nil), wantBytes, "the document grew")
}
