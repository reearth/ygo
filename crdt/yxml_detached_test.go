package crdt_test

import (
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A detached XML node reflects its buffered prelim content to readers, matching
// yjs's _prelimContent-aware length/toArray(): Len, Children, the attribute
// getters and ToXML all see what was inserted BEFORE the subtree attaches. The
// one exception is nested TEXT content — a detached YXmlText is opaque (Len 0,
// empty ToString), exactly as in yjs — so a detached element renders its tag
// and attributes but an empty body until it attaches and the text materialises.
func TestYXml_DetachedReadsReflectPrelim(t *testing.T) {
	doc := crdt.New()
	frag := doc.GetXmlFragment("f")

	el := crdt.NewYXmlElement("paragraph")
	txt := crdt.NewYXmlText()

	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		el.InsertText(txn, 0, txt)
		el.SetAttributeValue(txn, "level", 1)

		// Detached: children and attributes are visible; the text child is
		// present as a node but its content is still opaque.
		if n := el.Len(); n != 1 {
			t.Errorf("detached Len() = %d, want 1 (prelim child reflected)", n)
		}
		kids := el.Children()
		if len(kids) != 1 {
			t.Fatalf("detached Children() = %d nodes, want 1", len(kids))
		}
		if got, ok := kids[0].(*crdt.YXmlText); !ok || got != txt {
			t.Errorf("detached Children()[0] is not the inserted text node (identity lost)")
		}
		// Attribute value is reflected AND already normalized to the wire type
		// (int -> int64), so it reads identically before and after attach.
		if v, ok := el.GetAttributeValue("level"); !ok || v != int64(1) {
			t.Errorf("detached GetAttributeValue = %v (%T), %v; want int64(1), true", v, v, ok)
		}
		if attrs := el.GetAttributes(); len(attrs) != 1 || attrs["level"] != "1" {
			t.Errorf("detached GetAttributes = %v, want map[level:1]", attrs)
		}
		// Nested text opaque while detached -> empty body.
		if got, want := el.ToXML(), `<paragraph level="1"></paragraph>`; got != want {
			t.Errorf("detached ToXML() = %q, want %q", got, want)
		}
		if n := txt.Len(); n != 0 {
			t.Errorf("detached YXmlText Len() = %d, want 0 (text prelim is opaque)", n)
		}

		frag.InsertElement(txn, 0, el)
	}, nil)

	// Attached: the text materialised; the body fills in. Everything else is
	// unchanged from the detached reads — that's the point.
	// (Asserted OUTSIDE the transaction: ToXML on an attached tree recurses
	// into YXmlText.ToString, which takes the doc read lock that Transact
	// still holds inside the callback.)
	if n := el.Len(); n != 1 {
		t.Errorf("attached Len() = %d, want 1", n)
	}
	if v, ok := el.GetAttributeValue("level"); !ok || v != int64(1) {
		t.Errorf("attached GetAttributeValue = %v (%T), %v; want int64(1), true", v, v, ok)
	}
	if got, want := el.ToXML(), `<paragraph level="1">hello</paragraph>`; got != want {
		t.Errorf("attached ToXML() = %q, want %q (text now materialised)", got, want)
	}
	if got, want := frag.ToXML(), `<paragraph level="1">hello</paragraph>`; got != want {
		t.Errorf("final ToXML() = %q, want %q", got, want)
	}
}

// Reflection is recursive: a detached element built entirely from nested
// elements (no text) renders byte-for-byte the same ToXML detached as attached,
// proving Children()/attributes reflect prelim all the way down.
func TestYXml_DetachedNestedElementsReflectRecursively(t *testing.T) {
	doc := crdt.New()
	frag := doc.GetXmlFragment("f")

	ul := crdt.NewYXmlElement("bullet_list")
	li := crdt.NewYXmlElement("list_item")
	p := crdt.NewYXmlElement("paragraph")

	const want = `<bullet_list tight="true"><list_item><paragraph></paragraph></list_item></bullet_list>`

	doc.Transact(func(txn *crdt.Transaction) {
		li.InsertElement(txn, 0, p)
		ul.SetAttributeValue(txn, "tight", true)
		ul.InsertElement(txn, 0, li)

		// Detached, fully recursive read — no text anywhere, so ToXML is total.
		if got := ul.ToXML(); got != want {
			t.Errorf("detached recursive ToXML() = %q, want %q", got, want)
		}
		if n := ul.Len(); n != 1 {
			t.Errorf("detached ul.Len() = %d, want 1", n)
		}
		frag.InsertElement(txn, 0, ul)
	}, nil)

	// Identical after attach.
	if got := ul.ToXML(); got != want {
		t.Errorf("attached recursive ToXML() = %q, want %q", got, want)
	}
}

// Regression: because a detached element now reports its true Len(), the
// idiomatic append `el.Insert(txn, el.Len(), child)` appends in order. Under
// the previous empty-until-attached behavior Len() was always 0, so every
// insert landed at position 0 and the children came out reversed.
func TestYXml_DetachedInsertAtLenAppends(t *testing.T) {
	doc := crdt.New()
	frag := doc.GetXmlFragment("f")
	ul := crdt.NewYXmlElement("ul")

	doc.Transact(func(txn *crdt.Transaction) {
		for _, s := range []string{"a", "b", "c"} {
			txt := crdt.NewYXmlText()
			txt.Insert(txn, 0, s, nil)
			ul.InsertText(txn, ul.Len(), txt) // append at current end
		}
		frag.InsertElement(txn, 0, ul)
	}, nil)

	if got, want := ul.ToXML(), "<ul>abc</ul>"; got != want {
		t.Errorf("ToXML() = %q, want %q (Len()-relative append must preserve order)", got, want)
	}
}
