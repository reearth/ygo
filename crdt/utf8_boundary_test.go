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

func TestUnit_YText_RejectsInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff})
	newDoc := func() (*Doc, *YText) {
		d := New()
		return d, d.GetText("t")
	}

	t.Run("Insert content", func(t *testing.T) {
		d, txt := newDoc()
		requirePanicsWithInvalidUTF8(t, func() {
			d.Transact(func(txn *Transaction) { txt.Insert(txn, 0, bad, nil) })
		})
		require.Empty(t, txt.ToString())
	})
	t.Run("Insert attrs value", func(t *testing.T) {
		d, txt := newDoc()
		requirePanicsWithInvalidUTF8(t, func() {
			d.Transact(func(txn *Transaction) {
				txt.Insert(txn, 0, "ok", Attributes{"bold": bad})
			})
		})
		require.Empty(t, txt.ToString())
	})
	t.Run("Format attribute key", func(t *testing.T) {
		d, txt := newDoc()
		d.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
		requirePanicsWithInvalidUTF8(t, func() {
			d.Transact(func(txn *Transaction) { txt.Format(txn, 0, 5, Attributes{bad: "x"}) })
		})
	})
	t.Run("InsertEmbed value", func(t *testing.T) {
		d, txt := newDoc()
		requirePanicsWithInvalidUTF8(t, func() {
			d.Transact(func(txn *Transaction) {
				txt.InsertEmbed(txn, 0, map[string]any{"src": bad}, nil)
			})
		})
	})
	t.Run("ApplyDelta insert", func(t *testing.T) {
		d, txt := newDoc()
		requirePanicsWithInvalidUTF8(t, func() {
			d.Transact(func(txn *Transaction) {
				txt.ApplyDelta(txn, []Delta{{Op: DeltaOpInsert, Insert: bad}})
			})
		})
		require.Empty(t, txt.ToString())
	})
	t.Run("ApplyDelta attributes", func(t *testing.T) {
		d, txt := newDoc()
		requirePanicsWithInvalidUTF8(t, func() {
			d.Transact(func(txn *Transaction) {
				txt.ApplyDelta(txn, []Delta{{
					Op: DeltaOpInsert, Insert: "ok", Attributes: Attributes{"b": bad},
				}})
			})
		})
		require.Empty(t, txt.ToString())
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

// TestUnit_Doc_RootNamesRejectInvalidUTF8 guards the worst case in the whole
// UTF-8 plan: a bad ROOT TYPE NAME poisons the entire document (it's encoded
// on every update touching that root), not just one value. Covers all four
// root accessors plus the subdocument GUID (WithGUID).
func TestUnit_Doc_RootNamesRejectInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff})
	doc := New()
	for name, fn := range map[string]func(){
		"GetArray":       func() { doc.GetArray(bad) },
		"GetMap":         func() { doc.GetMap(bad) },
		"GetText":        func() { doc.GetText(bad) },
		"GetXmlFragment": func() { doc.GetXmlFragment(bad) },
	} {
		t.Run(name, func(t *testing.T) { requirePanicsWithInvalidUTF8(t, fn) })
	}
	t.Run("WithGUID", func(t *testing.T) {
		requirePanicsWithInvalidUTF8(t, func() { New(WithGUID(bad)) })
	})
	t.Run("valid unicode name still works", func(t *testing.T) {
		require.NotPanics(t, func() { New().GetText("документ 📄") })
	})
}

// TestUnit_Transaction_RootNamesRejectInvalidUTF8 covers the OTHER root-name
// entry point: Transaction.GetText/GetMap/GetArray/GetXmlFragment. These are
// the documented, recommended way to resolve a root type from INSIDE a
// Transact callback (Doc.GetText et al. would self-deadlock there — see
// GetText's doc comment and issue #138) — so this path sees more use inside
// transactions than Doc.GetX, not less. Each must panic independently of
// Doc.GetX's checks, since Transaction.GetX calls the lock-free
// t.doc.get*Locked helper directly rather than going through Doc.GetX.
//
// These necessarily run inside a Transact callback (that's the only place a
// *Transaction exists), which doubles as proof the panic propagates cleanly
// out of Transact without deadlocking (Transact has been panic-safe since
// v1.1.1).
func TestUnit_Transaction_RootNamesRejectInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff})
	for name, fn := range map[string]func(*Transaction){
		"GetText":        func(txn *Transaction) { txn.GetText(bad) },
		"GetMap":         func(txn *Transaction) { txn.GetMap(bad) },
		"GetArray":       func(txn *Transaction) { txn.GetArray(bad) },
		"GetXmlFragment": func(txn *Transaction) { txn.GetXmlFragment(bad) },
	} {
		t.Run(name, func(t *testing.T) {
			doc := New()
			requirePanicsWithInvalidUTF8(t, func() {
				doc.Transact(func(txn *Transaction) { fn(txn) })
			})
		})
	}
	t.Run("valid unicode name still works", func(t *testing.T) {
		doc := New()
		require.NotPanics(t, func() {
			doc.Transact(func(txn *Transaction) { txn.GetText("документ 📄") })
		})
	})
}

// TestUnit_YXml_RejectsInvalidUTF8 guards the three XML entry points that
// write a caller-supplied string to the wire: the node name passed to
// NewYXmlElement, and the attribute key/value pairs passed to SetAttribute
// and SetAttributeValue. NodeName is also an exported field a caller can
// assign directly, bypassing NewYXmlElement entirely — that path is NOT
// guarded here; Encoder.WriteVarString (Task 1) is the deliberate backstop
// for it, since adding a setter or reflection here would be chasing an
// unreachable corner for a struct field Yjs itself exposes as public.
func TestUnit_YXml_RejectsInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff})
	t.Run("NewYXmlElement node name", func(t *testing.T) {
		requirePanicsWithInvalidUTF8(t, func() { NewYXmlElement(bad) })
	})
	t.Run("SetAttribute key and value", func(t *testing.T) {
		doc := New()
		frag := doc.GetXmlFragment("f")
		el := NewYXmlElement("div")
		doc.Transact(func(txn *Transaction) { frag.InsertElement(txn, 0, el) })

		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { el.SetAttribute(txn, bad, "v") })
		})
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { el.SetAttribute(txn, "k", bad) })
		})
		_, ok := el.GetAttribute("k")
		require.False(t, ok, "no attribute may be committed by a rejected call")
	})
	t.Run("SetAttributeValue nested", func(t *testing.T) {
		doc := New()
		frag := doc.GetXmlFragment("f2")
		el := NewYXmlElement("div")
		doc.Transact(func(txn *Transaction) { frag.InsertElement(txn, 0, el) })
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) {
				el.SetAttributeValue(txn, "k", []any{bad})
			})
		})
	})
}

// TestUnit_YXmlElement_SetAttribute_Detached_RejectsInvalidUTF8 closes the
// same coverage gap Task 3 hit for YMap.Set: SetAttributeValue has an early
// `if t.detached()` branch that buffers into prelimAttrs, and Task 3's own
// history shows validation placed AFTER such a branch lets bad input sail
// through the buffer and materialise unvalidated at attach. Exercise the
// detached path directly (an element that was never inserted into a
// fragment) for both SetAttribute and SetAttributeValue, on both key and
// value.
func TestUnit_YXmlElement_SetAttribute_Detached_RejectsInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff})
	doc := New()

	t.Run("SetAttribute key", func(t *testing.T) {
		el := NewYXmlElement("div")
		require.True(t, el.detached())
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { el.SetAttribute(txn, bad, "v") })
		})
		_, ok := el.GetAttribute(bad)
		require.False(t, ok, "detached element must be untouched after a rejected call")
	})
	t.Run("SetAttribute value", func(t *testing.T) {
		el := NewYXmlElement("div")
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) { el.SetAttribute(txn, "k", bad) })
		})
		_, ok := el.GetAttribute("k")
		require.False(t, ok)
	})
	t.Run("SetAttributeValue nested value", func(t *testing.T) {
		el := NewYXmlElement("div")
		requirePanicsWithInvalidUTF8(t, func() {
			doc.Transact(func(txn *Transaction) {
				el.SetAttributeValue(txn, "k", []any{bad})
			})
		})
		_, ok := el.GetAttribute("k")
		require.False(t, ok)
	})
}

// TestUnit_YXml_ValidUnicodeStillWorks is the positive companion: emoji,
// non-Latin scripts and combining marks must still work as node names and
// attribute keys/values, both attached and detached.
func TestUnit_YXml_ValidUnicodeStillWorks(t *testing.T) {
	combining := "e" + "́"
	require.NotPanics(t, func() { NewYXmlElement("документ📄") })

	doc := New()
	frag := doc.GetXmlFragment("f")
	el := NewYXmlElement("タイトル")
	require.NotPanics(t, func() {
		doc.Transact(func(txn *Transaction) {
			frag.InsertElement(txn, 0, el)
			el.SetAttribute(txn, "🔑", "wörld")
			el.SetAttributeValue(txn, "n", combining)
		})
	})
	v, ok := el.GetAttribute("🔑")
	require.True(t, ok)
	require.Equal(t, "wörld", v)
}
