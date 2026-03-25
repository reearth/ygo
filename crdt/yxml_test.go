package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── YXmlFragment ──────────────────────────────────────────────────────────────

func TestUnit_YXmlFragment_InsertDelete(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")

	elem1 := NewYXmlElement("p")
	elem2 := NewYXmlElement("div")
	elem3 := NewYXmlElement("span")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem1, elem2)
	})

	assert.Equal(t, 2, frag.Len())
	require.Len(t, frag.Children(), 2)
	assert.Equal(t, elem1, frag.Children()[0].(*YXmlElement))
	assert.Equal(t, elem2, frag.Children()[1].(*YXmlElement))

	// Insert in the middle.
	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 1, elem3)
	})
	assert.Equal(t, 3, frag.Len())
	assert.Equal(t, elem3, frag.Children()[1].(*YXmlElement))

	// Delete the first child.
	doc.Transact(func(txn *Transaction) {
		frag.Delete(txn, 0, 1)
	})
	assert.Equal(t, 2, frag.Len())
	assert.Equal(t, elem3, frag.Children()[0].(*YXmlElement))
	assert.Equal(t, elem2, frag.Children()[1].(*YXmlElement))
}

func TestUnit_YXmlFragment_Delete_Range(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")

	e1 := NewYXmlElement("a")
	e2 := NewYXmlElement("b")
	e3 := NewYXmlElement("c")
	e4 := NewYXmlElement("d")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, e1, e2, e3, e4)
	})
	doc.Transact(func(txn *Transaction) {
		frag.Delete(txn, 1, 2) // remove e2 and e3
	})

	assert.Equal(t, 2, frag.Len())
	children := frag.Children()
	assert.Equal(t, e1, children[0].(*YXmlElement))
	assert.Equal(t, e4, children[1].(*YXmlElement))
}

func TestUnit_YXmlFragment_Observe_FiresOnce(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	calls := 0
	frag.Observe(func(e YXmlEvent) { calls++ })

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, NewYXmlElement("p"))
		frag.Insert(txn, 1, NewYXmlElement("div"))
	})

	assert.Equal(t, 1, calls, "observer must fire once per transaction")
}

func TestUnit_YXmlFragment_Observe_Unsubscribe(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	calls := 0
	unsub := frag.Observe(func(_ YXmlEvent) { calls++ })

	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, NewYXmlElement("p")) })
	unsub()
	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 1, NewYXmlElement("div")) })

	assert.Equal(t, 1, calls)
}

// ── YXmlElement ───────────────────────────────────────────────────────────────

func TestUnit_YXmlElement_Attributes(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("div")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.SetAttribute(txn, "class", "container")
		elem.SetAttribute(txn, "id", "main")
	})

	v, ok := elem.GetAttribute("class")
	assert.True(t, ok)
	assert.Equal(t, "container", v)

	v, ok = elem.GetAttribute("id")
	assert.True(t, ok)
	assert.Equal(t, "main", v)

	_, ok = elem.GetAttribute("missing")
	assert.False(t, ok)

	attrs := elem.GetAttributes()
	assert.Equal(t, map[string]string{"class": "container", "id": "main"}, attrs)
}

func TestUnit_YXmlElement_Attributes_Update(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("p")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.SetAttribute(txn, "class", "old")
	})
	doc.Transact(func(txn *Transaction) {
		elem.SetAttribute(txn, "class", "new")
	})

	v, ok := elem.GetAttribute("class")
	assert.True(t, ok)
	assert.Equal(t, "new", v)
	assert.Len(t, elem.GetAttributes(), 1)
}

func TestUnit_YXmlElement_Attributes_Delete(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("span")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.SetAttribute(txn, "style", "bold")
	})
	doc.Transact(func(txn *Transaction) {
		elem.DeleteAttribute(txn, "style")
	})

	_, ok := elem.GetAttribute("style")
	assert.False(t, ok)
	assert.Empty(t, elem.GetAttributes())
}

func TestUnit_YXmlElement_Observe(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("div")
	calls := 0
	elem.Observe(func(e YXmlEvent) { calls++ })

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.SetAttribute(txn, "class", "x")
	})

	assert.Equal(t, 1, calls)
}

// ── YXmlElement.ToXML ─────────────────────────────────────────────────────────

func TestUnit_YXmlElement_ToXML_Empty(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("br")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
	})

	assert.Equal(t, "<br></br>", frag.ToXML())
}

func TestUnit_YXmlElement_ToXML_WithAttributes(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("div")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.SetAttribute(txn, "class", "box")
		elem.SetAttribute(txn, "id", "main")
	})

	// Attributes are sorted alphabetically: class before id.
	assert.Equal(t, `<div class="box" id="main"></div>`, frag.ToXML())
}

func TestUnit_YXmlElement_ToXML_WithTextChild(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("p")
	text := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.Insert(txn, 0, text)
		text.Insert(txn, 0, "Hello, World!", nil)
	})

	assert.Equal(t, "<p>Hello, World!</p>", frag.ToXML())
}

func TestUnit_YXmlElement_ToXML_Nested(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")

	outer := NewYXmlElement("div")
	inner := NewYXmlElement("span")
	text := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, outer)
		outer.Insert(txn, 0, inner)
		inner.Insert(txn, 0, text)
		text.Insert(txn, 0, "nested", nil)
		outer.SetAttribute(txn, "class", "wrapper")
	})

	assert.Equal(t, `<div class="wrapper"><span>nested</span></div>`, frag.ToXML())
}

func TestUnit_YXmlElement_ToXML_XmlEscaping(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("p")
	text := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.Insert(txn, 0, text)
		text.Insert(txn, 0, "<em>bold</em> & \"quoted\"", nil)
	})

	assert.Equal(t, "<p>&lt;em&gt;bold&lt;/em&gt; &amp; \"quoted\"</p>", frag.ToXML())
}

func TestUnit_YXmlFragment_ToXML_MultipleSiblings(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")

	e1 := NewYXmlElement("h1")
	e2 := NewYXmlElement("p")
	t1 := NewYXmlText()
	t2 := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, e1, e2)
		e1.Insert(txn, 0, t1)
		t1.Insert(txn, 0, "Title", nil)
		e2.Insert(txn, 0, t2)
		t2.Insert(txn, 0, "Body", nil)
	})

	assert.Equal(t, "<h1>Title</h1><p>Body</p>", frag.ToXML())
}

// ── YXmlText ──────────────────────────────────────────────────────────────────

func TestUnit_YXmlText_InsertDelete(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("p")
	txt := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.Insert(txn, 0, txt)
		txt.Insert(txn, 0, "Hello World", nil)
	})
	assert.Equal(t, "Hello World", txt.ToString())

	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 5, 6)
	})
	assert.Equal(t, "Hello", txt.ToString())
	assert.Equal(t, "<p>Hello</p>", frag.ToXML())
}

func TestUnit_YXmlText_ToXML_Escaping(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("code")
	txt := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.Insert(txn, 0, txt)
		txt.Insert(txn, 0, "a < b && b > c", nil)
	})

	assert.Equal(t, "<code>a &lt; b &amp;&amp; b &gt; c</code>", frag.ToXML())
}

// ── Integration: concurrent edits ─────────────────────────────────────────────

func TestInteg_YXml_ConcurrentEdit_Convergence(t *testing.T) {
	// Two peers each insert an element into the same fragment concurrently.
	// Both must converge to an identical child list.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)

	frag1 := doc1.GetXmlFragment("root")
	frag2 := doc2.GetXmlFragment("root")

	peer1Elem := NewYXmlElement("p")
	peer2Elem := NewYXmlElement("div")

	doc1.Transact(func(txn *Transaction) { frag1.Insert(txn, 0, peer1Elem) })
	doc2.Transact(func(txn *Transaction) { frag2.Insert(txn, 0, peer2Elem) })

	// Exchange: apply doc1's new items into doc2 and vice-versa.
	// For ContentType items, clone the YXmlElement (preserving NodeName) so
	// Children() returns correctly-typed nodes in the destination doc.
	applyXmlItems := func(src *Doc, dst *Doc, dstFrag *YXmlFragment) {
		dst.Transact(func(txn *Transaction) {
			src.store.IterateFrom(dst.store.StateVector(), func(item *Item) {
				if item.ParentSub != "" {
					return // skip attribute items for this test
				}
				var cloneContent Content
				if ct, ok := item.Content.(*ContentType); ok {
					if origElem, ok := ct.Type.owner.(*YXmlElement); ok {
						cloneElem := NewYXmlElement(origElem.NodeName)
						cloneElem.YXmlFragment.abstractType.doc = dst
						cloneContent = NewContentType(&cloneElem.YXmlFragment.abstractType)
					} else {
						cloneContent = item.Content.Copy()
					}
				} else {
					cloneContent = item.Content.Copy()
				}

				var left *Item
				if item.Origin != nil {
					left = dst.store.Find(*item.Origin)
				}
				clone := &Item{
					ID:          item.ID,
					Origin:      item.Origin,
					OriginRight: item.OriginRight,
					Left:        left,
					Parent:      &dstFrag.abstractType,
					Content:     cloneContent,
				}
				clone.integrate(txn, 0)
			})
		})
	}

	applyXmlItems(doc1, doc2, frag2)
	applyXmlItems(doc2, doc1, frag1)

	assert.Equal(t, frag1.Len(), frag2.Len(), "child counts must converge")
	assert.Equal(t, 2, frag1.Len())

	// Verify order: lower ClientID (doc1=1) sorts before doc2=2 per YATA.
	children1 := frag1.Children()
	children2 := frag2.Children()
	require.Len(t, children1, 2)
	require.Len(t, children2, 2)
	assert.Equal(t, "p", children1[0].(*YXmlElement).NodeName)
	assert.Equal(t, "div", children1[1].(*YXmlElement).NodeName)
	// Both peers must agree on order.
	assert.Equal(t, children1[0].(*YXmlElement).NodeName, children2[0].(*YXmlElement).NodeName)
	assert.Equal(t, children1[1].(*YXmlElement).NodeName, children2[1].(*YXmlElement).NodeName)
}
