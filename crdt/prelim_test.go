package crdt

import (
	"encoding/json"
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
	// Still detached, so nothing has materialised.
	if got := len(m.Keys()); got != 0 {
		t.Fatalf("detached map has %d keys, want 0 (Sets must buffer)", got)
	}

	doc.Transact(func(txn *Transaction) { root.PushType(txn, m) })
	if got := len(m.Keys()); got != 2 {
		t.Fatalf("attached map has %d keys, want 2 (buffered Sets must replay)", got)
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
