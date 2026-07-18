package crdt

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the detached (prelim) mutation paths of the XML types and
// the best-effort attribute string rendering — the Yjs-parity surface added
// by the XML wire-conformance work (#147). The detached/attached EQUIVALENCE
// assertions are the interesting part: buffering while detached must be
// semantically transparent once the subtree attaches.

// Insert on a detached fragment/element clamps an out-of-range index to
// "append", matching a JS splice on Yjs's _prelimContent.
func TestUnit_YXml_DetachedInsertClampsIndex(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	el := NewYXmlElement("ul")
	a, b, c := NewYXmlText(), NewYXmlText(), NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		a.Insert(txn, 0, "a", nil)
		b.Insert(txn, 0, "b", nil)
		c.Insert(txn, 0, "c", nil)
		el.InsertText(txn, 99, a) // beyond end → append: [a]
		el.InsertText(txn, -3, b) // negative → append: [a b]
		el.InsertText(txn, 1, c)  // in range: [a c b]
		frag.InsertElement(txn, 0, el)
	})

	assert.Equal(t, "<ul>acb</ul>", frag.ToXML())
}

// Delete on a detached fragment/element splices the buffered prelim children
// (Yjs YXmlFragment.delete on _prelimContent): out-of-range and non-positive
// requests are no-ops, and an overlong length is clamped to the buffer end.
func TestUnit_YXml_DetachedDeleteSplicesPrelim(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	el := NewYXmlElement("p")
	a, b, c := NewYXmlText(), NewYXmlText(), NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		a.Insert(txn, 0, "a", nil)
		b.Insert(txn, 0, "b", nil)
		c.Insert(txn, 0, "c", nil)
		el.InsertText(txn, 0, a)
		el.InsertText(txn, 1, b)
		el.InsertText(txn, 2, c) // [a b c]
		el.Delete(txn, 1, 1)     // [a c]
		el.Delete(txn, 5, 1)     // index past end → no-op
		el.Delete(txn, -1, 2)    // negative index → no-op
		el.Delete(txn, 0, 0)     // non-positive length → no-op
		el.Delete(txn, 1, 10)    // length clamped to end → [a]
		frag.InsertElement(txn, 0, el)
	})

	assert.Equal(t, "<p>a</p>", frag.ToXML())
}

// Attributes set while detached upsert in place (JS Map parity: re-setting a
// key keeps its position) and DeleteAttribute removes a buffered attribute;
// deleting an absent key is a no-op.
func TestUnit_YXml_DetachedAttrUpsertAndDelete(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	el := NewYXmlElement("heading")

	doc.Transact(func(txn *Transaction) {
		el.SetAttributeValue(txn, "level", 1)
		el.SetAttributeValue(txn, "id", "x")
		el.SetAttributeValue(txn, "level", 2) // upsert existing prelim key
		el.SetAttribute(txn, "title", "T")    // string wrapper, still detached
		el.DeleteAttribute(txn, "id")         // removes the buffered attribute
		el.DeleteAttribute(txn, "ghost")      // absent key → no-op
		frag.InsertElement(txn, 0, el)
	})

	// NewContentAny normalizes Go ints to int64 (wire-number parity).
	assert.Equal(t, map[string]any{"level": int64(2), "title": "T"}, el.GetAttributeValues())
	_, ok := el.GetAttributeValue("id")
	assert.False(t, ok, "attribute deleted while detached must not materialise")
}

// GetAttribute/GetAttributes render non-string values best-effort (documented
// as NOT JavaScript String() parity: Go's %v formatting for exponents,
// negative zero, and composites). The typed values stay reachable through
// GetAttributeValue.
func TestUnit_YXmlElement_AttrStringRendering(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	el := NewYXmlElement("n")

	doc.Transact(func(txn *Transaction) {
		frag.InsertElement(txn, 0, el)
		el.SetAttributeValue(txn, "s", "str")
		el.SetAttributeValue(txn, "n", 42)
		el.SetAttributeValue(txn, "f", 1.5)
		el.SetAttributeValue(txn, "i", 3.0)
		el.SetAttributeValue(txn, "b", true)
		el.SetAttributeValue(txn, "z", nil)
		el.SetAttributeValue(txn, "tiny", 1e-7)
		el.SetAttributeValue(txn, "big", 1e20)
		el.SetAttributeValue(txn, "neg0", math.Copysign(0, -1))
		el.SetAttributeValue(txn, "list", []any{int64(1), "x"})
		el.SetAttributeValue(txn, "obj", map[string]any{})
	})

	want := map[string]string{
		"s":    "str",
		"n":    "42",
		"f":    "1.5",
		"i":    "3", // integral float: no trailing ".0"
		"b":    "true",
		"z":    "",      // nil renders empty
		"tiny": "1e-07", // Go %v — JS String() would give "1e-7"
		"big":  "1e+20", // Go %v — JS String() would give "100000000000000000000"
		"neg0": "-0",
		"list": "[1 x]",
		"obj":  "map[]",
	}
	assert.Equal(t, want, el.GetAttributes())
	for k, w := range want {
		got, ok := el.GetAttribute(k)
		require.True(t, ok, "GetAttribute(%q) missing", k)
		assert.Equal(t, w, got, "GetAttribute(%q)", k)
	}
	// The nil-valued attribute is PRESENT (ok=true) with a nil typed value.
	v, ok := el.GetAttributeValue("z")
	require.True(t, ok)
	assert.Nil(t, v)
}

// Typed attribute values must survive the wire in both update formats: encode
// the authoring doc as V1 and V2, apply each to a fresh doc, and require the
// identical typed attribute map (V2 exercises the un-deduplicated key path).
func TestUnit_YXmlElement_TypedAttrsWireRoundTrip(t *testing.T) {
	src := newTestDoc(7)
	frag := src.GetXmlFragment("root")
	el := NewYXmlElement("heading")

	src.Transact(func(txn *Transaction) {
		el.SetAttributeValue(txn, "level", 2)
		el.SetAttributeValue(txn, "ratio", 1.5)
		el.SetAttributeValue(txn, "flag", true)
		el.SetAttributeValue(txn, "name", "n")
		el.SetAttributeValue(txn, "tiny", 1e-7)
		el.SetAttributeValue(txn, "big", 1e20)
		frag.InsertElement(txn, 0, el)
	})

	require.Equal(t, map[string]any{
		"level": int64(2),
		"ratio": 1.5,
		"flag":  true,
		"name":  "n",
		"tiny":  1e-7,
		"big":   1e20,
	}, el.GetAttributeValues())

	// On the wire, lib0 narrows a float64 to float32 (Any tag 124) when the
	// value round-trips losslessly; the decoder keeps that width. So 1.5
	// comes back as float32 while 1e-7 / 1e20 (not float32-lossless) stay
	// float64 — same numeric values in both formats.
	wantDecoded := map[string]any{
		"level": int64(2),
		"ratio": float32(1.5),
		"flag":  true,
		"name":  "n",
		"tiny":  1e-7,
		"big":   1e20,
	}
	for _, tc := range []struct {
		tag    string
		encode func(*Doc, StateVector) []byte
		apply  func(*Doc, []byte, any) error
	}{
		{"v1", EncodeStateAsUpdateV1, ApplyUpdateV1},
		{"v2", EncodeStateAsUpdateV2, ApplyUpdateV2},
	} {
		dst := New()
		require.NoError(t, tc.apply(dst, tc.encode(src, nil), nil), tc.tag)
		kids := dst.GetXmlFragment("root").Children()
		require.Len(t, kids, 1, tc.tag)
		got := kids[0].(*YXmlElement)
		assert.Equal(t, "heading", got.NodeName, tc.tag)
		assert.Equal(t, wantDecoded, got.GetAttributeValues(), tc.tag)
	}
}

// Every buffered YText mutation (Insert, Format, InsertEmbed, Delete,
// ApplyDelta) issued on a DETACHED text node must produce, after attach,
// exactly the state that the same operations produce on an attached twin.
func TestUnit_YXmlText_DetachedBufferedOpsMatchAttached(t *testing.T) {
	doc := newTestDoc(3)
	frag := doc.GetXmlFragment("root")
	det, att := NewYXmlText(), NewYXmlText()

	ops := func(txn *Transaction, txt *YXmlText) {
		txt.Insert(txn, 0, "hello world", nil)
		txt.Format(txn, 0, 5, Attributes{"bold": map[string]any{}})
		txt.InsertEmbed(txn, 11, map[string]any{"src": "x.png"}, nil)
		txt.Delete(txn, 5, 6) // drop " world"; the embed shifts left
		txt.ApplyDelta(txn, []Delta{
			{Op: DeltaOpRetain, Retain: 5},
			{Op: DeltaOpInsert, Insert: "!"},
		})
	}

	doc.Transact(func(txn *Transaction) {
		// Attached twin: attach first, operations apply directly.
		frag.InsertText(txn, 0, att)
		ops(txn, att)
		// Detached twin: operations buffer, attach flushes them in order.
		ops(txn, det)
		frag.InsertText(txn, 1, det)
	})

	assert.Equal(t, 7, det.Len(), "6 UTF-16 units + 1 embed")
	assert.Equal(t, att.Len(), det.Len())
	assert.Equal(t, att.ToString(), det.ToString())
	assert.Equal(t, att.ToDelta(), det.ToDelta(),
		"detached buffering must be semantically transparent")
}
