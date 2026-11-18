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
// object.equalFlat is one level: the same number of keys, each compared with
// ===. So two objects with equal contents are equal, but a nested object is
// compared by identity, and a number compares by value whatever its Go type.
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

		// One level only: a nested object compares by identity.
		{"nested objects, equal contents", map[string]any{"x": map[string]any{"y": 1}}, map[string]any{"x": map[string]any{"y": 1}}, false},
		{"nested object, same reference", map[string]any{"x": nested}, map[string]any{"x": nested}, true},
		{"same map reference", shared, shared, true},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, attrEqual(c.a, c.b), "attrEqual(%v, %v) [%s]", c.a, c.b, c.name)
		assert.Equal(t, c.want, attrEqual(c.b, c.a), "attrEqual is symmetric: %s", c.name)
	}
}
