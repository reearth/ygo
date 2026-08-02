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
	doc := New()
	m := doc.GetMap("m")
	a := doc.GetArray("a")
	require.NotPanics(t, func() {
		doc.Transact(func(txn *Transaction) {
			m.Set(txn, "🔑", map[string]any{"n": []any{"héllo", "😀"}})
			a.Push(txn, []any{"ok", "wörld"})
		})
	})
	require.Equal(t, 2, a.Len())
}
