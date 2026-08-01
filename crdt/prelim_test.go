package crdt

import (
	"encoding/json"
	"fmt"
	"testing"
	"unicode/utf8"
)

// buildCell constructs the shape a Jupyter notebook cell has — a Y.Map holding a
// Y.Text — which is the case these constructors exist for.
func buildCell(doc *Doc, arrName, text string) {
	cells := doc.GetArray(arrName)
	doc.Transact(func(txn *Transaction) {
		cell := NewMapPrelim()
		src := NewTextPrelim()
		src.Insert(txn, 0, text, nil)
		cell.Set(txn, "cell_type", "markdown")
		cell.Set(txn, "source", src)
		cell.Set(txn, "metadata", NewMapPrelim())
		cells.PushType(txn, cell)
	})
}

func TestPrelimMapBuffersSetsUntilAttached(t *testing.T) {
	doc := New()
	root := doc.GetArray("root") // hoisted: GetArray locks the doc, Transact holds that lock
	m := NewMapPrelim()

	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "a", "1")
		m.Set(txn, "b", "2")
	})
	// Staged, so the map reads back its own content — but nothing has
	// materialised into the document yet.
	if got := m.Keys(); len(got) != 2 {
		t.Fatalf("detached map has keys %v, want 2 (staged Sets must be readable)", got)
	}
	if v, ok := m.Get("a"); !ok || v != "1" {
		t.Fatalf(`detached Get("a") = %v, %v; want "1", true`, v, ok)
	}
	if !m.Has("b") {
		t.Fatal(`detached Has("b") = false, want true`)
	}
	if got := root.Len(); got != 0 {
		t.Fatalf("root has %d items before attach, want 0 (staging must not materialise)", got)
	}

	doc.Transact(func(txn *Transaction) { root.PushType(txn, m) })
	if got := len(m.Keys()); got != 2 {
		t.Fatalf("attached map has %d keys, want 2 (staged Sets must materialise)", got)
	}
}

func TestDetachedReadsUnwrapStagedNestedTypes(t *testing.T) {
	// A staged nested type reads back as its live handle, and renders through
	// ToJSON, exactly as it would once attached.
	doc := New()
	outer := NewMapPrelim()
	inner := NewMapPrelim()
	list := NewArrayPrelim()
	el := NewYXmlElement("div")
	xtext := NewYXmlText()
	arr := NewArrayPrelim()
	doc.Transact(func(txn *Transaction) {
		inner.Set(txn, "deep", "value")
		list.Push(txn, []any{1.0, 2.0})
		outer.Set(txn, "inner", inner)
		outer.Set(txn, "list", list)
		outer.Set(txn, "el", el)
		outer.Set(txn, "xtext", xtext)
		arr.PushType(txn, inner)
	})

	if got, ok := outer.Get("inner"); !ok || got != any(inner) {
		t.Fatalf(`detached Get("inner") = %v, %v; want the staged *YMap handle`, got, ok)
	}
	if got := arr.Get(0); got != any(inner) {
		t.Fatalf("detached array Get(0) = %v, want the staged *YMap handle", got)
	}

	b, err := outer.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("detached ToJSON %s: %v", b, err)
	}
	nested, isMap := got["inner"].(map[string]any)
	if !isMap || nested["deep"] != "value" {
		t.Fatalf("inner rendered as %v, want {deep: value}", got["inner"])
	}
	if items, isArr := got["list"].([]any); !isArr || len(items) != 2 {
		t.Fatalf("list rendered as %v, want two items", got["list"])
	}
	if s, isStr := got["el"].(string); !isStr || s == "" {
		t.Fatalf("el rendered as %v, want XML markup", got["el"])
	}
	if _, isStr := got["xtext"].(string); !isStr {
		t.Fatalf("xtext rendered as %v, want a string", got["xtext"])
	}
}

func TestPrelimReadsSurviveTheAttachBoundary(t *testing.T) {
	// The same reads must answer identically either side of attach — the point
	// of staging state rather than buffering calls.
	doc := New()
	root := doc.GetArray("root")
	cell := NewMapPrelim()
	body := NewTextPrelim()
	inner := NewArrayPrelim()
	doc.Transact(func(txn *Transaction) {
		body.Insert(txn, 0, "note", nil)
		inner.Push(txn, []any{"x", "y"})
		cell.Set(txn, "source", body)
		cell.Set(txn, "outputs", inner)
		cell.Set(txn, "cell_type", "markdown")
	})

	before, err := cell.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *Transaction) { root.PushType(txn, cell) })
	after, err := cell.ToJSON()
	if err != nil {
		t.Fatal(err)
	}

	var b, a map[string]any
	if err := json.Unmarshal(before, &b); err != nil {
		t.Fatalf("detached ToJSON %s: %v", before, err)
	}
	if err := json.Unmarshal(after, &a); err != nil {
		t.Fatal(err)
	}
	// YText stages deferred ops (matching Yjs's Y.Text._pending), so its
	// detached content is not readable; the map and array staging is.
	if b["cell_type"] != "markdown" {
		t.Fatalf("detached cell_type = %v, want markdown", b["cell_type"])
	}
	if got, want := fmt.Sprint(b["outputs"]), fmt.Sprint(a["outputs"]); got != want {
		t.Fatalf("outputs read %s detached and %s attached; want identical", got, want)
	}
	if a["source"] != "note" {
		t.Fatalf("attached source = %v, want note", a["source"])
	}
}

func TestPrelimTextBuffersInsertUntilAttached(t *testing.T) {
	doc := New()
	root := doc.GetArray("root")
	txt := NewTextPrelim()
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		root.PushType(txn, txt)
	})
	b, err := txt.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"hello"` {
		t.Fatalf("text = %s, want \"hello\"", b)
	}
}

func TestMapGetReturnsLiveNestedHandles(t *testing.T) {
	doc := New()
	buildCell(doc, "cells", "coach note")

	cell, ok := doc.GetArray("cells").Get(0).(*YMap)
	if !ok {
		t.Fatalf("cells[0] is %T, want *YMap", doc.GetArray("cells").Get(0))
	}
	// The regression this fixes: these read back as (nil, false) before the
	// ContentType branch in Get.
	src, ok := cell.Get("source")
	if !ok {
		t.Fatal(`Get("source") not ok — nested types must be reachable`)
	}
	if _, isText := src.(*YText); !isText {
		t.Fatalf("source is %T, want *YText (a real nested type, not a blob)", src)
	}
	meta, ok := cell.Get("metadata")
	if !ok {
		t.Fatal(`Get("metadata") not ok`)
	}
	if _, isMap := meta.(*YMap); !isMap {
		t.Fatalf("metadata is %T, want *YMap", meta)
	}
	if ct, _ := cell.Get("cell_type"); ct != "markdown" {
		t.Fatalf("cell_type = %v, want markdown (scalars must still work)", ct)
	}
}

func TestNestedPrelimRoundTripsThroughAnUpdate(t *testing.T) {
	src := New()
	buildCell(src, "cells", "coach note")

	dst := New()
	if err := dst.ApplyUpdate(src.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	got, err := dst.GetArray("cells").ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var cells []map[string]any
	if err := json.Unmarshal(got, &cells); err != nil {
		t.Fatalf("decoding %s: %v", got, err)
	}
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1", len(cells))
	}
	if cells[0]["source"] != "coach note" {
		t.Fatalf("source = %v, want \"coach note\"", cells[0]["source"])
	}
	if cells[0]["cell_type"] != "markdown" {
		t.Fatalf("cell_type = %v, want markdown", cells[0]["cell_type"])
	}
}

func TestChildClocksLandAboveTheContainerClock(t *testing.T) {
	// The ordering requirement the buffering exists for: a child item created
	// BEFORE its container would carry a lower clock, which genuine Yjs never
	// produces and Y.applyUpdate cannot decode. Proven by round-tripping through
	// the wire format, which is where such an update would fail.
	src := New()
	buildCell(src, "cells", "x")
	update := src.EncodeStateAsUpdate()

	dst := New()
	if err := dst.ApplyUpdate(update); err != nil {
		t.Fatalf("update did not decode — child/container clock ordering is wrong: %v", err)
	}
	if got := dst.GetArray("cells").Len(); got != 1 {
		t.Fatalf("decoded %d cells, want 1", got)
	}
}

func TestPushTypeRejectsAnAttachedType(t *testing.T) {
	doc := New()
	a, b := doc.GetArray("a"), doc.GetArray("b")
	m := NewMapPrelim()
	doc.Transact(func(txn *Transaction) { a.PushType(txn, m) })

	defer func() {
		if recover() == nil {
			t.Fatal("PushType accepted an already-attached type; want panic")
		}
	}()
	doc.Transact(func(txn *Transaction) { b.PushType(txn, m) })
}

func TestPushTypeAppendsAfterExistingItems(t *testing.T) {
	// A notebook is many cells, so consecutive PushType calls must anchor each
	// new item after the previous physical tail — the non-empty-array path in
	// PushType, which the single-cell tests never reach.
	src := New()
	cells := src.GetArray("cells")
	src.Transact(func(txn *Transaction) {
		for _, text := range []string{"first", "second", "third"} {
			cell := NewMapPrelim()
			body := NewTextPrelim()
			body.Insert(txn, 0, text, nil)
			cell.Set(txn, "source", body)
			cells.PushType(txn, cell)
		}
	})

	dst := New()
	if err := dst.ApplyUpdate(src.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	got, err := dst.GetArray("cells").ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decoding %s: %v", got, err)
	}
	if len(decoded) != 3 {
		t.Fatalf("got %d cells, want 3", len(decoded))
	}
	for i, want := range []string{"first", "second", "third"} {
		if decoded[i]["source"] != want {
			t.Fatalf("cells[%d].source = %v, want %q (push order must hold)", i, decoded[i]["source"], want)
		}
	}
}

func TestPrelimArrayBuffersMutationsUntilAttached(t *testing.T) {
	// Insert, Delete and Move on a detached array must buffer and replay on
	// attach, like Push — otherwise their items would carry clocks below the
	// container's. Verified end to end: the update must decode elsewhere.
	doc := New()
	root := doc.GetArray("root")
	arr := NewArrayPrelim()
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"b", "d"})
		arr.Insert(txn, 0, []any{"a"})
		arr.Insert(txn, 2, []any{"c"}) // a b c d
		arr.Delete(txn, 3, 1)          // a b c
		arr.Move(txn, 0, 3)            // b c a
	})
	// Staged, so the array reads back its own content; nothing has
	// materialised into the document yet.
	if got := arr.Len(); got != 3 {
		t.Fatalf("detached array has len %d, want 3 (staged mutations must be readable)", got)
	}
	if got := arr.Get(0); got != "b" {
		t.Fatalf("detached Get(0) = %v, want b", got)
	}
	if got := root.Len(); got != 0 {
		t.Fatalf("root has %d items before attach, want 0 (staging must not materialise)", got)
	}

	doc.Transact(func(txn *Transaction) { root.PushType(txn, arr) })
	want := []any{"b", "c", "a"}
	assertSlice := func(label string, a *YArray) {
		t.Helper()
		got := a.ToSlice()
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want %v", label, got, want)
			}
		}
	}
	assertSlice("attached array", arr)

	dst := New()
	if err := dst.ApplyUpdate(doc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	inner, ok := dst.GetArray("root").Get(0).(*YArray)
	if !ok {
		t.Fatalf("root[0] is %T, want *YArray", dst.GetArray("root").Get(0))
	}
	assertSlice("decoded array", inner)
}

func TestPushTypeBuffersWhenArrayDetached(t *testing.T) {
	// A nested type pushed into a DETACHED array must buffer like every other
	// mutation, or the child materialises with a clock below the container's —
	// an update ygo silently decodes to an empty root and real Yjs rejects.
	doc := New()
	root := doc.GetArray("root")
	outer := NewArrayPrelim()
	inner := NewMapPrelim()
	doc.Transact(func(txn *Transaction) {
		inner.Set(txn, "k", "v")
		outer.PushType(txn, inner)
		root.PushType(txn, outer)
	})

	dst := New()
	if err := dst.ApplyUpdate(doc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	got, err := dst.GetArray("root").ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[[{"k":"v"}]]` {
		t.Fatalf("decoded root = %s, want [[{\"k\":\"v\"}]]", got)
	}
}

func TestSetRejectsAnAttachedType(t *testing.T) {
	// An attached type stored again would fall through to ContentAny and only
	// blow up at encode time — inside Doc.Transact when an OnUpdate hook is
	// registered. Reject it loudly at Set, like PushType already does.
	doc := New()
	m := doc.GetMap("m")
	child := NewTextPrelim()
	doc.Transact(func(txn *Transaction) { m.Set(txn, "a", child) })

	defer func() {
		if recover() == nil {
			t.Fatal("Set accepted an already-attached type; want panic")
		}
	}()
	doc.Transact(func(txn *Transaction) { m.Set(txn, "b", child) })
}

func TestPushRejectsASharedType(t *testing.T) {
	// Push(txn, []any{prelim}) is the literal translation of Yjs's
	// cells.push([cell]) — it must fail loudly and point at PushType, not
	// store a blob that panics the encoder later.
	doc := New()
	root := doc.GetArray("root")
	defer func() {
		if recover() == nil {
			t.Fatal("Push accepted a shared type as a plain value; want panic")
		}
	}()
	doc.Transact(func(txn *Transaction) { root.Push(txn, []any{NewMapPrelim()}) })
}

func TestInsertRejectsASharedType(t *testing.T) {
	doc := New()
	root := doc.GetArray("root")
	defer func() {
		if recover() == nil {
			t.Fatal("Insert accepted a shared type as a plain value; want panic")
		}
	}()
	doc.Transact(func(txn *Transaction) { root.Insert(txn, 0, []any{NewTextPrelim()}) })
}

func TestPrelimMapDeleteBuffersUntilAttached(t *testing.T) {
	// Delete on a detached map must buffer like Set, or the buffered Set
	// replays at attach and resurrects the deleted key. Yjs yields {} here.
	doc := New()
	root := doc.GetArray("root")
	m := NewMapPrelim()
	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "a", "1")
		m.Delete(txn, "a")
	})
	doc.Transact(func(txn *Transaction) { root.PushType(txn, m) })
	if got := len(m.Keys()); got != 0 {
		t.Fatalf("attached map has %d keys, want 0 (buffered Delete must replay after Set)", got)
	}

	dst := New()
	if err := dst.ApplyUpdate(doc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	got, err := dst.GetArray("root").ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[{}]` {
		t.Fatalf("decoded root = %s, want [{}]", got)
	}
}

func TestPrelimArrayNestsInsideAMap(t *testing.T) {
	doc := New()
	root := doc.GetArray("root")
	outer := NewMapPrelim()
	doc.Transact(func(txn *Transaction) {
		inner := NewArrayPrelim()
		inner.Push(txn, []any{"one", "two"})
		outer.Set(txn, "outputs", inner)
		root.PushType(txn, outer)
	})
	v, ok := outer.Get("outputs")
	if !ok {
		t.Fatal(`Get("outputs") not ok`)
	}
	arr, isArr := v.(*YArray)
	if !isArr {
		t.Fatalf("outputs is %T, want *YArray", v)
	}
	if arr.Len() != 2 {
		t.Fatalf("outputs len = %d, want 2", arr.Len())
	}
}

// FuzzPrelimNestedRoundTrip builds arbitrary nested prelim shapes, attaches
// them, and requires the encoded update to decode into an equivalent document.
// The property under test is wire-ordering: a child materialised before its
// container carries a lower clock, which genuine Yjs never emits and
// Y.applyUpdate cannot decode — so a failure here surfaces as a decode error.
func FuzzPrelimNestedRoundTrip(f *testing.F) {
	f.Add("hello", "markdown", 2)
	f.Add("", "code", 0)
	f.Add("multi\nline\ttext", "raw", 5)

	f.Fuzz(func(t *testing.T, text, kind string, n int) {
		if n < 0 || n > 32 {
			t.Skip()
		}
		// Constrain to valid UTF-8. Go strings may hold arbitrary bytes, but
		// ygo's varstring encoding rejects non-UTF-8 on decode — a pre-existing
		// property unrelated to prelim construction, and letting it through
		// would make this target test string encoding instead of wire ordering.
		if !utf8.ValidString(text) || !utf8.ValidString(kind) {
			t.Skip()
		}
		src := New()
		cells := src.GetArray("cells")
		src.Transact(func(txn *Transaction) {
			cell := NewMapPrelim()
			body := NewTextPrelim()
			body.Insert(txn, 0, text, nil)
			cell.Set(txn, "cell_type", kind)
			cell.Set(txn, "source", body)

			outputs := NewArrayPrelim()
			for i := 0; i < n; i++ {
				outputs.Push(txn, []any{"o"})
			}
			cell.Set(txn, "outputs", outputs)
			cells.PushType(txn, cell)
		})

		dst := New()
		if err := dst.ApplyUpdate(src.EncodeStateAsUpdate()); err != nil {
			t.Fatalf("update failed to decode (child/container clock ordering): %v", err)
		}
		if got := dst.GetArray("cells").Len(); got != 1 {
			t.Fatalf("decoded %d cells, want 1", got)
		}
	})
}
