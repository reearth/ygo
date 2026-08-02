package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_YMap_Set_RejectsInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff})
	t.Run("key", func(t *testing.T) {
		doc := New()
		m := doc.GetMap("m")
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { m.Set(txn, bad, "v") })
		})
		require.Empty(t, m.Keys(), "document must be untouched after a rejected call")
	})
	t.Run("value", func(t *testing.T) {
		doc := New()
		m := doc.GetMap("m")
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { m.Set(txn, "k", bad) })
		})
		require.Empty(t, m.Keys())
	})
	t.Run("nested value", func(t *testing.T) {
		doc := New()
		m := doc.GetMap("m")
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) {
				m.Set(txn, "k", map[string]any{"inner": []any{bad}})
			})
		})
		require.Empty(t, m.Keys())
	})
}

func TestUnit_YArray_RejectsInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff})
	t.Run("Push validates before mutating", func(t *testing.T) {
		doc := New()
		a := doc.GetArray("a")
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { a.Push(txn, []any{"ok", bad}) })
		})
		require.Zero(t, a.Len(), "the valid leading value must not be committed")
	})
	t.Run("Insert", func(t *testing.T) {
		doc := New()
		a := doc.GetArray("a")
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { a.Insert(txn, 0, []any{bad}) })
		})
		require.Zero(t, a.Len())
	})
}

func TestUnit_YMapYArray_ValidUnicodeStillWorks(t *testing.T) {
	// "héllo" uses precomposed U+00E9; combining is a decomposed sequence
	// ("e" + U+0301 COMBINING ACUTE ACCENT) — two valid-UTF-8 byte shapes for
	// what looks like the same character, both of which must pass unchanged.
	combining := "e" + "́"
	doc := New()
	m := doc.GetMap("m")
	a := doc.GetArray("a")
	require.NotPanics(t, func() {
		doc.Transact(func(txn *Transaction) {
			m.Set(txn, "🔑", map[string]any{"n": []any{"héllo", "😀", combining}})
			a.Push(txn, []any{"ok", "wörld"})
		})
	})
	require.Equal(t, 2, a.Len())
}

// TestUnit_YMap_Set_Detached_RejectsInvalidUTF8 closes a coverage gap: the
// tests above all use doc.GetMap, which attaches immediately, so t.detached()
// is never true and nothing would fail if the checks were ever moved past
// the `if t.detached()` branch in YMap.Set. NewMapPrelim (v1.43.0's public
// prelim constructor) is the real, documented way to reach that branch — it
// stages content, and the wire only sees it once the type attaches — so a
// regression here would ship invalid UTF-8 in the staged/attach path.
func TestUnit_YMap_Set_Detached_RejectsInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff})
	doc := New()

	t.Run("key", func(t *testing.T) {
		m := NewMapPrelim()
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { m.Set(txn, bad, "v") })
		})
		require.Empty(t, m.Keys(), "detached map must be untouched after a rejected call")
	})
	t.Run("value", func(t *testing.T) {
		m := NewMapPrelim()
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { m.Set(txn, "k", bad) })
		})
		require.Empty(t, m.Keys())
	})
}

// TestUnit_YMap_Set_Detached_ValidUTF8MaterialisesOnAttach is the companion
// positive case: a detached map built with valid (including decomposed
// combining-mark) content must still stage correctly and materialise
// unchanged once attached — proving the new checks do not break the staged
// path they guard.
func TestUnit_YMap_Set_Detached_ValidUTF8MaterialisesOnAttach(t *testing.T) {
	combining := "e" + "́"
	doc := New()
	parent := doc.GetMap("parent") // hoisted: GetMap locks the doc, Transact holds that lock

	child := NewMapPrelim()
	doc.Transact(func(txn *Transaction) {
		child.Set(txn, "🔑", combining)
		parent.Set(txn, "child", child)
	})

	// child is the same *YMap; item.integrate attached it in place.
	require.Equal(t, []string{"🔑"}, child.Keys())
	v, ok := child.Get("🔑")
	require.True(t, ok)
	require.Equal(t, combining, v)
}
