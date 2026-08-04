package crdt

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_CheckUTF8_PanicMessageNamesLocation(t *testing.T) {
	require.PanicsWithValue(t, `crdt: YMap.Set: key: invalid UTF-8`,
		func() { checkUTF8("YMap.Set", "key", string([]byte{0xff})) })
	require.NotPanics(t, func() { checkUTF8("YMap.Set", "key", "fine 😀") })
}

func TestUnit_CheckAnyUTF8_WalksNestedShapes(t *testing.T) {
	bad := string([]byte{0xff})
	for name, v := range map[string]any{
		"plain string":  bad,
		"slice element": []any{"ok", bad},
		"map value":     map[string]any{"k": bad},
		"map key":       map[string]any{bad: "ok"},
		"nested deep":   []any{map[string]any{"a": []any{"ok", bad}}},
	} {
		t.Run(name, func(t *testing.T) {
			requirePanicsWithInvalidUTF8(t, func() { checkAnyUTF8("op", "value", v) })
		})
	}
}

func TestUnit_CheckAnyUTF8_IgnoresNonStringValues(t *testing.T) {
	for name, v := range map[string]any{
		"int": 42, "float": 1.5, "bool": true, "nil": nil,
		"bytes": []byte{0xff, 0xfe}, // []byte is length-prefixed, not UTF-8
	} {
		t.Run(name, func(t *testing.T) {
			require.NotPanics(t, func() { checkAnyUTF8("op", "value", v) })
		})
	}
}

// helper, defined in this file
func requirePanicsWithInvalidUTF8(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		require.NotNil(t, r, "expected a panic")
		require.Contains(t, r.(string), "invalid UTF-8")
	}()
	fn()
}

func TestUnit_CheckAnyUTF8_ReportsIndexAndKey(t *testing.T) {
	bad := string([]byte{0xff})
	requirePanicsWithMessage(t, `crdt: op: value[1]: invalid UTF-8`,
		func() { checkAnyUTF8("op", "value", []any{"ok", bad}) })
	requirePanicsWithMessage(t, `crdt: op: value["k"]: invalid UTF-8`,
		func() { checkAnyUTF8("op", "value", map[string]any{"k": bad}) })
}

func requirePanicsWithMessage(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		require.NotNil(t, r, "expected a panic")
		require.Equal(t, want, r)
	}()
	fn()
}

func TestUnit_CheckAttrsUTF8_PanicsOnInvalidKey(t *testing.T) {
	bad := string([]byte{0xff})
	requirePanicsWithMessage(t, fmt.Sprintf("crdt: YText.Format: attribute key %q: invalid UTF-8", bad),
		func() { checkAttrsUTF8("YText.Format", Attributes{bad: "ok"}) })
}

func TestUnit_CheckAttrsUTF8_PanicsOnInvalidValue(t *testing.T) {
	bad := string([]byte{0xff})
	requirePanicsWithMessage(t, `crdt: YText.ApplyDelta: attribute "k": invalid UTF-8`,
		func() { checkAttrsUTF8("YText.ApplyDelta", Attributes{"k": bad}) })
}

func TestUnit_CheckAttrsUTF8_WalksNestedValue(t *testing.T) {
	bad := string([]byte{0xff})
	requirePanicsWithInvalidUTF8(t, func() {
		checkAttrsUTF8("YText.Insert", Attributes{"k": []any{"ok", bad}})
	})
}

func TestUnit_CheckAttrsUTF8_IgnoresValidAttrs(t *testing.T) {
	require.NotPanics(t, func() {
		checkAttrsUTF8("YText.Format", Attributes{"bold": true, "color": "red", "size": 12})
	})
}

// Guards the walker against WriteAny growing a new string-bearing case.
func TestUnit_CheckAnyUTF8_CoversWriteAnyStringCases(t *testing.T) {
	src, err := os.ReadFile("../encoding/encoder.go")
	require.NoError(t, err)
	body := string(src)
	start := strings.Index(body, "func (e *Encoder) WriteAny(")
	require.Greater(t, start, -1)
	end := strings.Index(body[start:], "\n}\n")
	require.Greater(t, end, -1)
	cases := body[start : start+end]

	// Only these three carry text. If a new one appears, extend checkAnyUTF8.
	for _, want := range []string{"case string:", "case []any:", "case map[string]any:"} {
		require.Contains(t, cases, want)
	}
	require.Equal(t, 19, strings.Count(cases, "\tcase "),
		"WriteAny's type switch changed — re-check whether the new case carries strings, then update this count")
}
