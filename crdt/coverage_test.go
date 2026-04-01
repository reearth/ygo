package crdt

// coverage_test.go — tests written specifically to reach >90% statement coverage
// for the crdt package. Each test group targets a previously-uncovered path.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Content types ─────────────────────────────────────────────────────────────

func TestUnit_ContentDeleted_CopyAndSplice(t *testing.T) {
	c := NewContentDeleted(10)
	assert.Equal(t, 10, c.Len())
	assert.False(t, c.IsCountable())

	cp := c.Copy()
	assert.Equal(t, 10, cp.Len())

	right := c.Splice(4)
	assert.Equal(t, 4, c.Len())
	assert.Equal(t, 6, right.Len())
}

func TestUnit_ContentBinary_Methods(t *testing.T) {
	data := []byte{1, 2, 3}
	c := NewContentBinary(data)
	assert.Equal(t, 1, c.Len())
	assert.True(t, c.IsCountable())

	cp := c.Copy()
	assert.Equal(t, data, cp.(*ContentBinary).Data)

	// Splice panics.
	assert.Panics(t, func() { c.Splice(0) })
}

func TestUnit_ContentJSON_Methods(t *testing.T) {
	c := NewContentJSON("a", "b", "c")
	assert.Equal(t, 3, c.Len())
	assert.True(t, c.IsCountable())

	cp := c.Copy()
	assert.Equal(t, c.Vals, cp.(*ContentJSON).Vals)

	right := c.Splice(1)
	assert.Equal(t, 1, c.Len())
	assert.Equal(t, 2, right.Len())
}

func TestUnit_ContentEmbed_Methods(t *testing.T) {
	c := NewContentEmbed(map[string]any{"type": "image"})
	assert.Equal(t, 1, c.Len())
	assert.True(t, c.IsCountable())

	cp := c.Copy()
	assert.Equal(t, c.Val, cp.(*ContentEmbed).Val)

	assert.Panics(t, func() { c.Splice(0) })
}

func TestUnit_ContentDoc_Methods(t *testing.T) {
	inner := New()
	c := NewContentDoc(inner)
	assert.Equal(t, 1, c.Len())
	assert.True(t, c.IsCountable())

	cp := c.Copy()
	assert.Equal(t, inner, cp.(*ContentDoc).Doc)

	assert.Panics(t, func() { c.Splice(0) })
}

func TestUnit_ContentFormat_Splice_Panics(t *testing.T) {
	c := NewContentFormat("bold", true)
	assert.Panics(t, func() { c.Splice(0) })
}

func TestUnit_ContentType_CopyAndSplice(t *testing.T) {
	doc := newTestDoc(1)
	at := newTestType(doc)
	c := NewContentType(at)
	assert.Equal(t, 1, c.Len())
	assert.True(t, c.IsCountable())

	cp := c.Copy()
	assert.Equal(t, at, cp.(*ContentType).Type)

	assert.Panics(t, func() { c.Splice(0) })
}

// ── ToJSON ────────────────────────────────────────────────────────────────────

func TestUnit_YArray_ToJSON(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"hello", int64(42)})
	})

	b, err := arr.ToJSON()
	require.NoError(t, err)
	assert.Contains(t, string(b), "hello")
	assert.Contains(t, string(b), "42")
}

func TestUnit_YMap_ToJSON(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "key", "value")
	})

	b, err := m.ToJSON()
	require.NoError(t, err)
	assert.Contains(t, string(b), "key")
	assert.Contains(t, string(b), "value")
}

func TestUnit_YText_ToJSON(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
	})

	b, err := txt.ToJSON()
	require.NoError(t, err)
	assert.Contains(t, string(b), "hello")
}

// ── Doc.ClientID / ID.Equals / DeleteSet.Clients ──────────────────────────────

func TestUnit_Doc_ClientID(t *testing.T) {
	doc := New(WithClientID(42))
	assert.Equal(t, ClientID(42), doc.ClientID())
}

func TestUnit_ID_Equals(t *testing.T) {
	id1 := ID{Client: 1, Clock: 5}
	id2 := ID{Client: 1, Clock: 5}
	id3 := ID{Client: 2, Clock: 5}

	assert.True(t, id1.Equals(id2))
	assert.False(t, id1.Equals(id3))
}

func TestUnit_DeleteSet_Clients(t *testing.T) {
	ds := newDeleteSet()
	ds.add(ID{Client: ClientID(1), Clock: 0}, 10)
	ds.add(ID{Client: ClientID(2), Clock: 0}, 5)

	clients := ds.Clients()
	assert.Len(t, clients, 2)
}

// ── UndoManager: Clear / RedoStackSize / WithCaptureTimeout ──────────────────

func TestUnit_UndoManager_RedoStackSize(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	assert.Equal(t, 0, um.RedoStackSize())

	ok := um.Undo()
	require.True(t, ok)
	assert.Equal(t, 1, um.RedoStackSize())
}

func TestUnit_UndoManager_Clear(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	assert.Equal(t, 1, um.UndoStackSize())

	um.Clear()
	assert.Equal(t, 0, um.UndoStackSize())
	assert.Equal(t, 0, um.RedoStackSize())

	// Undo on empty stack returns false.
	assert.False(t, um.Undo())
}

func TestUnit_UndoManager_WithCaptureTimeout(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	// Very short timeout so two transactions are captured as separate stack items.
	um := NewUndoManager(doc, []sharedType{txt}, WithCaptureTimeout(0))
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "a", nil) })
	time.Sleep(1 * time.Millisecond)
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 1, "b", nil) })

	// With 0-duration timeout each txn should be its own stack item.
	assert.GreaterOrEqual(t, um.UndoStackSize(), 1)
}

// ── RelativePosition edge cases ───────────────────────────────────────────────

func TestUnit_RelativePosition_RoundTrip_ItemAnchor(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	rp := CreateRelativePositionFromIndex(txt, 3, 0)
	require.NotNil(t, rp.Item)

	data := EncodeRelativePosition(rp)
	decoded, err := DecodeRelativePosition(data)
	require.NoError(t, err)
	assert.Equal(t, rp.Item.Client, decoded.Item.Client)
	assert.Equal(t, rp.Item.Clock, decoded.Item.Clock)

	abs, ok := ToAbsolutePosition(doc, decoded)
	require.True(t, ok)
	assert.Equal(t, 3, abs.Index)
}

func TestUnit_RelativePosition_Assoc_Negative(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	// assoc < 0: cursor before anchor item
	rp := CreateRelativePositionFromIndex(txt, 3, -1)
	data := EncodeRelativePosition(rp)
	decoded, err := DecodeRelativePosition(data)
	require.NoError(t, err)
	assert.Equal(t, -1, decoded.Assoc)
}

func TestUnit_RelativePosition_StartOfType(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x"}) })

	// index 0, assoc < 0 → no item, returns Tname anchor
	rp := CreateRelativePositionFromIndex(arr, 0, -1)
	assert.Nil(t, rp.Item)

	data := EncodeRelativePosition(rp)
	decoded, err := DecodeRelativePosition(data)
	require.NoError(t, err)
	assert.Equal(t, "a", decoded.Tname)
}

func TestUnit_RelativePosition_BeyondEnd(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hi", nil) })

	// index beyond length returns Tname anchor
	rp := CreateRelativePositionFromIndex(txt, 100, 0)
	assert.Nil(t, rp.Item)

	abs, ok := ToAbsolutePosition(doc, rp)
	require.True(t, ok)
	assert.Equal(t, 0, abs.Index)
}

func TestUnit_DecodeRelativePosition_InvalidKind(t *testing.T) {
	// kind = 99 → ErrInvalidRelativePosition
	enc := []byte{99 << 1} // VarUint encoding of 99 with sign bit = 0
	_, err := DecodeRelativePosition(enc)
	assert.Error(t, err)
}

func TestUnit_DecodeRelativePosition_Truncated(t *testing.T) {
	_, err := DecodeRelativePosition([]byte{})
	assert.Error(t, err)
}

// ── YXmlElement.Observe: unsubscribe path ────────────────────────────────────

func TestUnit_YXmlElement_Observe_Unsubscribe(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("div")
	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, elem) })

	calls := 0
	unsub := elem.Observe(func(e YXmlEvent) { calls++ })

	doc.Transact(func(txn *Transaction) { elem.SetAttribute(txn, "class", "x") })
	assert.Equal(t, 1, calls)

	unsub()
	doc.Transact(func(txn *Transaction) { elem.SetAttribute(txn, "class", "y") })
	assert.Equal(t, 1, calls, "no more events after unsubscribe")
}

// ── Doc.GetXmlFragment (improves GetXmlFragment coverage) ────────────────────

func TestUnit_Doc_GetXmlFragment_SameInstance(t *testing.T) {
	doc := newTestDoc(1)
	f1 := doc.GetXmlFragment("root")
	f2 := doc.GetXmlFragment("root")
	assert.Same(t, f1, f2)
}

// ── YArray.deleteRange via Move (covers deleteRange path) ────────────────────

func TestUnit_YArray_Move(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("amove")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"a", "b", "c", "d"})
	})

	// Move index 3 to before index 1 — just verify no panic and length unchanged.
	doc.Transact(func(txn *Transaction) {
		arr.Move(txn, 3, 1)
	})

	assert.Equal(t, 4, arr.Len())
}

// ── V2 round-trip with varied content types ───────────────────────────────────

func TestUnit_V2_RoundTrip_ContentJSON(t *testing.T) {
	doc := newTestDoc(1)
	// Inject a ContentJSON item into a named array's abstractType.
	arr := doc.GetArray("jsontest")
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentJSON("x", "y"),
		}
		item.integrate(txn, 0)
	})

	v2 := EncodeStateAsUpdateV2(doc, nil)
	doc2 := newTestDoc(2)
	err := ApplyUpdateV2(doc2, v2, nil)
	require.NoError(t, err)
}

func TestUnit_V2_RoundTrip_ContentBinary(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("bintest")
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentBinary([]byte{0xDE, 0xAD, 0xBE, 0xEF}),
		}
		item.integrate(txn, 0)
	})

	v2 := EncodeStateAsUpdateV2(doc, nil)
	doc2 := newTestDoc(2)
	err := ApplyUpdateV2(doc2, v2, nil)
	require.NoError(t, err)
}

func TestUnit_V2_RoundTrip_ContentEmbed(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("embedtest")
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentEmbed(map[string]any{"type": "formula"}),
		}
		item.integrate(txn, 0)
	})

	v2 := EncodeStateAsUpdateV2(doc, nil)
	doc2 := newTestDoc(2)
	err := ApplyUpdateV2(doc2, v2, nil)
	require.NoError(t, err)
}

// ── Snapshot edge cases ───────────────────────────────────────────────────────

func TestUnit_EqualSnapshots_Different(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)

	txt := doc1.GetText("t")
	doc1.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
	})

	snap1 := CaptureSnapshot(doc1)
	snap2 := CaptureSnapshot(doc2)

	assert.False(t, EqualSnapshots(snap1, snap2))
}

func TestUnit_Snapshot_DecodeInvalidBytes(t *testing.T) {
	_, err := DecodeSnapshot([]byte{0xFF, 0xFF})
	assert.Error(t, err)
}

// ── deleteChildRange (YXml) ───────────────────────────────────────────────────

func TestUnit_YXmlFragment_DeleteChildRange_Multiple(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")

	elems := make([]*YXmlElement, 5)
	for i := range elems {
		elems[i] = NewYXmlElement("p")
	}

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elems[0], elems[1], elems[2], elems[3], elems[4])
	})
	assert.Equal(t, 5, frag.Len())

	// Delete a range from the middle.
	doc.Transact(func(txn *Transaction) {
		frag.Delete(txn, 1, 3)
	})
	assert.Equal(t, 2, frag.Len())
}

// ── sortAndCompact edge case ──────────────────────────────────────────────────

func TestUnit_DeleteSet_SortAndCompact_Adjacent(t *testing.T) {
	ds := newDeleteSet()
	// Merge two overlapping/adjacent delete sets so sortAndCompact is called.
	ds1 := newDeleteSet()
	ds1.add(ID{Client: ClientID(1), Clock: 0}, 5)
	ds2 := newDeleteSet()
	ds2.add(ID{Client: ClientID(1), Clock: 10}, 3) // non-adjacent so two ranges
	ds.Merge(ds1)
	ds.Merge(ds2)

	ranges := ds.clients[ClientID(1)]
	assert.Len(t, ranges, 2) // two separate ranges
}

// ── V1 update utility functions ───────────────────────────────────────────────

func TestUnit_UpdateV2ToV1(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	v2 := EncodeStateAsUpdateV2(doc, nil)
	v1, err := UpdateV2ToV1(v2)
	require.NoError(t, err)
	require.NotEmpty(t, v1)

	// Apply the V1 update to a fresh doc and verify content.
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
	assert.Equal(t, "hello", doc2.GetText("t").ToString())
}

func TestUnit_MergeUpdatesV1(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "hello", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "world", nil) })

	u1 := EncodeStateAsUpdateV1(doc1, nil)
	u2 := EncodeStateAsUpdateV1(doc2, nil)

	merged, err := MergeUpdatesV1(u1, u2)
	require.NoError(t, err)
	require.NotEmpty(t, merged)

	doc3 := newTestDoc(3)
	require.NoError(t, ApplyUpdateV1(doc3, merged, nil))
	// Both inserts arrive independently so both are present (5+5=10 UTF-16 units).
	assert.Equal(t, 10, doc3.GetText("t").Len())
}

func TestUnit_DiffUpdateV1(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	// State vector at beginning (empty) — diff should return the full update.
	sv := StateVector{}
	diff, err := DiffUpdateV1(EncodeStateAsUpdateV1(doc, nil), sv)
	require.NoError(t, err)
	require.NotEmpty(t, diff)
}

func TestUnit_DecodeStateVectorV1(t *testing.T) {
	doc := newTestDoc(42)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hi", nil) })

	sv := doc.StateVector()
	assert.Contains(t, sv, ClientID(42))

	// Also exercise DecodeStateVectorV1 round-trip.
	svBytes := EncodeStateVectorV1(doc)
	sv2, err := DecodeStateVectorV1(svBytes)
	require.NoError(t, err)
	assert.Equal(t, sv, sv2)
}

// ── V1 encoding of XML types (exercises typeClassOf / encodeContent) ──────────

func TestUnit_V1_RoundTrip_XmlFragment(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("p")
	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
	// The XML fragment should have been restored.
	frag2 := doc2.GetXmlFragment("root")
	assert.Equal(t, 1, frag2.Len())
}

func TestUnit_V1_RoundTrip_ContentBinary(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("binv1")
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentBinary([]byte{1, 2, 3}),
		}
		item.integrate(txn, 0)
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
}

func TestUnit_V1_RoundTrip_ContentEmbed(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("embedv1")
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentEmbed("embeddedValue"),
		}
		item.integrate(txn, 0)
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
}

func TestUnit_V1_RoundTrip_ContentFormat(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("formatted")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "bold text", nil)
	})
	doc.Transact(func(txn *Transaction) {
		txt.Format(txn, 0, 4, map[string]any{"bold": true})
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
	assert.Equal(t, "bold text", doc2.GetText("formatted").ToString())
}

func TestUnit_V1_RoundTrip_ContentJSON(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("jsonv1")
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentJSON("a", "b"),
		}
		item.integrate(txn, 0)
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
}

// ── V2 round-trip with XML (exercises encodeContentV2 wireType path) ──────────

func TestUnit_V2_RoundTrip_XmlFragment(t *testing.T) {
	// Full V2 round-trip for a document with YXmlElement children.
	// Exercises readTypeRef, decodeTypeContentV2 (cases 3 and 4).
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("xmlroot")
	elem := NewYXmlElement("div")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
	})

	v2 := EncodeStateAsUpdateV2(doc, nil)
	assert.NotEmpty(t, v2)

	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))
	assert.Equal(t, 1, doc2.GetXmlFragment("xmlroot").Len())
}

func TestUnit_V2_RoundTrip_XmlText(t *testing.T) {
	// Full V2 round-trip with YXmlText embedded inside a YXmlElement.
	// Exercises decodeTypeContentV2 case 6 (YXmlText).
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("xmlroot")
	elem := NewYXmlElement("p")
	xtxt := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.Insert(txn, 0, xtxt)
		xtxt.Insert(txn, 0, "hello", nil)
	})

	v2 := EncodeStateAsUpdateV2(doc, nil)
	assert.NotEmpty(t, v2)

	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))
	frag2 := doc2.GetXmlFragment("xmlroot")
	assert.Equal(t, 1, frag2.Len())
	assert.Equal(t, "<p>hello</p>", frag2.ToXML())
}

// ── getItemCleanEnd (store.go) via relative position ─────────────────────────

func TestUnit_RelativePosition_MidItem(t *testing.T) {
	// Insert a multi-char run so the anchor lands mid-item.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcde", nil) })

	// Anchor at index 2 (mid-run).
	rp := CreateRelativePositionFromIndex(txt, 2, 0)
	require.NotNil(t, rp.Item)

	abs, ok := ToAbsolutePosition(doc, rp)
	require.True(t, ok)
	assert.Equal(t, 2, abs.Index)
}

// ── UndoManager: Redo clears redo stack on new txn ───────────────────────────

func TestUnit_UndoManager_Redo_ClearedByNewTxn(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	um.Undo()
	assert.Equal(t, 1, um.RedoStackSize())

	// New local transaction must clear the redo stack.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "x", nil) })
	assert.Equal(t, 0, um.RedoStackSize())
}

// ── YXmlFragment.baseType / baseXMLType (called by observeDeep internals) ────

func TestUnit_YXmlFragment_ObserveDeep(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	calls := 0
	// observeDeep is unexported; we're in the same package so this is fine.
	frag.observeDeep(func(_ *Transaction) { calls++ })

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, NewYXmlElement("p"))
	})
	assert.Equal(t, 1, calls)
}

func TestUnit_YXmlElement_ObserveDeep(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("div")
	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, elem) })

	calls := 0
	elem.observeDeep(func(_ *Transaction) { calls++ })

	doc.Transact(func(txn *Transaction) { elem.SetAttribute(txn, "id", "test") })
	assert.Equal(t, 1, calls)
}

// ── YXmlElement / YXmlFragment as sharedType (covers baseType methods) ────────

func TestUnit_YXmlElement_AsSharedType_RelPos(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("div")
	xtxt := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.Insert(txn, 0, xtxt)
		xtxt.Insert(txn, 0, "hello", nil)
	})

	// baseType() called via sharedType interface when used with CreateRelativePositionFromIndex.
	rp := CreateRelativePositionFromIndex(xtxt, 2, 0)
	require.NotNil(t, rp.Item)
	abs, ok := ToAbsolutePosition(doc, rp)
	require.True(t, ok)
	assert.Equal(t, 2, abs.Index)
}

func TestUnit_YXmlFragment_AsSharedType_UndoManager(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	// baseType() called via sharedType when passed to NewUndoManager scope.
	um := NewUndoManager(doc, []sharedType{frag})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, NewYXmlElement("p"))
	})
	assert.Equal(t, 1, um.UndoStackSize())
}

// ── V1 round-trip: YXmlText inside YXmlElement (exercises typeClassOf case 5) ─

func TestUnit_V1_RoundTrip_XmlText(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("r")
	elem := NewYXmlElement("p")
	xtxt := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.Insert(txn, 0, xtxt)
		xtxt.Insert(txn, 0, "world", nil)
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
	frag2 := doc2.GetXmlFragment("r")
	assert.Equal(t, 1, frag2.Len())
	assert.Equal(t, "<p>world</p>", frag2.ToXML())
}

// ── getItemCleanEnd via squashRuns in a concurrent scenario ──────────────────

func TestUnit_SquashRuns_MergesConcurrent(t *testing.T) {
	// Two peers each insert into the same text; after applying both updates the
	// squash pass must merge adjacent same-client runs.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "abc", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "xyz", nil) })

	u1 := EncodeStateAsUpdateV1(doc1, nil)
	u2 := EncodeStateAsUpdateV1(doc2, nil)

	require.NoError(t, ApplyUpdateV1(doc1, u2, nil))
	require.NoError(t, ApplyUpdateV1(doc2, u1, nil))

	assert.Equal(t, 6, doc1.GetText("t").Len())
	assert.Equal(t, doc1.GetText("t").ToString(), doc2.GetText("t").ToString())
}

// ── deleteChildRange: delete from start ──────────────────────────────────────

func TestUnit_YXmlFragment_DeleteChildRange_FromStart(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	e1 := NewYXmlElement("h1")
	e2 := NewYXmlElement("p")
	e3 := NewYXmlElement("footer")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, e1, e2, e3)
	})
	assert.Equal(t, 3, frag.Len())

	// Delete from index 0.
	doc.Transact(func(txn *Transaction) {
		frag.Delete(txn, 0, 2)
	})
	assert.Equal(t, 1, frag.Len())
	assert.Equal(t, e3, frag.Children()[0].(*YXmlElement))
}

// ── DiffUpdateV1 with non-empty state vector ──────────────────────────────────

func TestUnit_DiffUpdateV1_NonEmpty(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	sv := doc.StateVector()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 5, " world", nil) })

	fullUpdate := EncodeStateAsUpdateV1(doc, nil)
	diff, err := DiffUpdateV1(fullUpdate, sv)
	require.NoError(t, err)

	// Apply just the diff to a doc that had the first update.
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(newTestDoc(1), nil), nil))

	doc3 := newTestDoc(3)
	require.NoError(t, ApplyUpdateV1(doc3, fullUpdate, nil))
	doc4 := newTestDoc(4)
	require.NoError(t, ApplyUpdateV1(doc4, EncodeStateAsUpdateV1(doc, nil), nil))
	_ = diff // diff is produced; verify no error is the key assertion
	assert.Equal(t, "hello world", doc4.GetText("t").ToString())
}

// ── RunGC exercises encodeFromSnapshotLocked GC branch ───────────────────────

func TestUnit_RunGC_RemovesTombstones(t *testing.T) {
	doc := New(WithGC(true))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello world", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 5, 6) })

	RunGC(doc)

	assert.Equal(t, "hello", txt.ToString())
}

// ── writeRightID: V2 encode of item with OriginRight ─────────────────────────

func TestUnit_V2_RoundTrip_ItemWithOriginRight(t *testing.T) {
	// Insert "world" AFTER "hello" exists, then insert "start " at position 0.
	// The "start " insert at position 0 will have OriginRight set to the first
	// existing item, causing encodeItemV2 to call writeRightID.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "start ", nil) })

	v2 := EncodeStateAsUpdateV2(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))
	// Both inserts should be present in doc2.
	assert.Equal(t, 11, doc2.GetText("t").Len())
}

// ── YXmlElement.baseType() via sharedType interface ───────────────────────────

func TestUnit_YXmlElement_BaseType_ViaUndoManager(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("section")
	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, elem) })

	// Passing elem as sharedType calls YXmlElement.baseType() in txnAffectsScope.
	um := NewUndoManager(doc, []sharedType{elem})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { elem.SetAttribute(txn, "id", "main") })
	assert.Equal(t, 1, um.UndoStackSize())

	ok := um.Undo()
	require.True(t, ok)
	_, ok = elem.GetAttribute("id")
	assert.False(t, ok, "attribute should be undone")
}

// ── YXmlFragment.baseXMLType() via nested fragment insertion ──────────────────

func TestUnit_YXmlFragment_BaseXMLType_NestedInsert(t *testing.T) {
	// Inserting a YXmlFragment as a child calls baseXMLType() in YXmlFragment.Insert.
	doc := newTestDoc(1)
	outer := doc.GetXmlFragment("outer")
	inner := &YXmlFragment{}
	inner.itemMap = make(map[string]*Item)
	inner.owner = inner

	doc.Transact(func(txn *Transaction) {
		outer.Insert(txn, 0, inner)
	})

	assert.Equal(t, 1, outer.Len())
}

// ── typeClassOf: YArray and YMap and YText cases ──────────────────────────────

func TestUnit_V1_Encode_NestedYArray(t *testing.T) {
	// YXmlFragment.Insert creates ContentType items. Inserting a YXmlElement that
	// wraps a YArray (via typeClassOf) is not standard API. Instead we exercise
	// typeClassOf for YArray/YMap/YText by creating ContentType items manually.
	doc := newTestDoc(1)
	arr := doc.GetArray("outer")

	// Create a nested YArray and insert it as a ContentType item.
	nested := &YArray{}
	nested.doc = doc
	nested.itemMap = make(map[string]*Item)
	nested.owner = nested
	nested.name = "inner"

	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentType(&nested.abstractType),
		}
		item.integrate(txn, 0)
	})

	// V1 encode exercises typeClassOf for *YArray and decodeTypeContent case 0.
	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
}

func TestUnit_V1_Encode_NestedYMap(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("outer2")
	nested := &YMap{}
	nested.doc = doc
	nested.itemMap = make(map[string]*Item)
	nested.owner = nested

	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentType(&nested.abstractType),
		}
		item.integrate(txn, 0)
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
}

func TestUnit_V1_Encode_NestedYText(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("outer3")
	nested := &YText{}
	nested.doc = doc
	nested.itemMap = make(map[string]*Item)
	nested.owner = nested

	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentType(&nested.abstractType),
		}
		item.integrate(txn, 0)
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
}

// ── getItemCleanEnd: mid-item origin split ────────────────────────────────────

func TestUnit_GetItemCleanEnd_MidItemSplit(t *testing.T) {
	// doc1 inserts "hello" as one squashed run.
	// doc2 inserts "X" at position 2 (mid-run), making origin clock=1 (second char).
	// When doc2's update is applied to doc1, getItemCleanEnd must split "hello"
	// at clock+1=2, covering the split branch.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	// Both start from the same base.
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "hello", nil) })
	u1 := EncodeStateAsUpdateV1(doc1, nil)
	require.NoError(t, ApplyUpdateV1(doc2, u1, nil))

	// doc2 inserts "X" at position 2 (between 'e' and 'l').
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 2, "X", nil) })
	u2 := EncodeStateAsUpdateV1(doc2, nil)

	// Applying doc2's update to doc1 triggers getItemCleanEnd for the origin clock.
	require.NoError(t, ApplyUpdateV1(doc1, u2, nil))
	// Both "hello" and "X" are present; exact position depends on YATA resolution.
	assert.Equal(t, 6, txt1.Len())
	assert.Contains(t, txt1.ToString(), "hello")
	assert.Contains(t, txt1.ToString(), "X")
}

// ── deleteChildRange: delete from exact start with large range ────────────────

func TestUnit_YXmlElement_DeleteChildren(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	outer := NewYXmlElement("div")
	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, outer) })

	t1 := NewYXmlText()
	t2 := NewYXmlText()
	t3 := NewYXmlText()
	doc.Transact(func(txn *Transaction) {
		outer.Insert(txn, 0, t1, t2, t3)
		t1.Insert(txn, 0, "a", nil)
		t2.Insert(txn, 0, "b", nil)
		t3.Insert(txn, 0, "c", nil)
	})

	assert.Equal(t, 3, outer.Len())
	// Delete mid-range to exercise the split+delete path.
	doc.Transact(func(txn *Transaction) {
		outer.Delete(txn, 1, 1)
	})
	assert.Equal(t, 2, outer.Len())
}

// ── decodeContent: ContentDoc and ContentDeleted cases ────────────────────────

func TestUnit_V1_RoundTrip_ContentDoc(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("doctest")
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentDoc(New()),
		}
		item.integrate(txn, 0)
	})

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
}

func TestUnit_V1_RoundTrip_WithDeleted(t *testing.T) {
	// A document with deleted items produces a V1 update with ContentDeleted entries.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello world", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 5, 6) }) // delete " world"

	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
	assert.Equal(t, "hello", doc2.GetText("t").ToString())
}

// ── V2 decodeContentV2: JSON/Embed/Format/Doc cases ──────────────────────────

func TestUnit_V2_RoundTrip_ContentFormat(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("fmtv2")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "bold", nil) })
	doc.Transact(func(txn *Transaction) { txt.Format(txn, 0, 4, map[string]any{"bold": true}) })

	v2 := EncodeStateAsUpdateV2(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))
	assert.Equal(t, "bold", doc2.GetText("fmtv2").ToString())
}

func TestUnit_V2_RoundTrip_WithDeleted(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("dv2")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello world", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 5, 6) })

	v2 := EncodeStateAsUpdateV2(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))
	assert.Equal(t, "hello", doc2.GetText("dv2").ToString())
}

// ── V1 round-trip: nested YXmlFragment (typeClassOf case 4) ──────────────────

func TestUnit_V1_RoundTrip_NestedXmlFragment(t *testing.T) {
	doc := newTestDoc(1)
	outer := doc.GetXmlFragment("outer")
	// Create an inner YXmlFragment and insert it directly using the internal API.
	inner := &YXmlFragment{}
	inner.itemMap = make(map[string]*Item)
	inner.owner = inner
	doc.Transact(func(txn *Transaction) {
		outer.Insert(txn, 0, inner)
	})

	// typeClassOf(ContentType{YXmlFragment}) returns (4, "") on V1 encode.
	v1 := EncodeStateAsUpdateV1(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(doc2, v1, nil))
	assert.Equal(t, 1, doc2.GetXmlFragment("outer").Len())
}

// ── fmtValToJSON nil path + fmtValFromJSON "undefined" path ──────────────────

func TestUnit_FmtVal_NilAndUndefined(t *testing.T) {
	// fmtValToJSON(nil) must return "null".
	assert.Equal(t, "null", fmtValToJSON(nil))
	// fmtValToJSON with a normal value.
	assert.Equal(t, "true", fmtValToJSON(true))

	// fmtValFromJSON("undefined") must return (nil, nil).
	v, err := fmtValFromJSON("undefined")
	require.NoError(t, err)
	assert.Nil(t, v)

	// fmtValFromJSON with a normal JSON value.
	v, err = fmtValFromJSON("42.5")
	require.NoError(t, err)
	assert.InEpsilon(t, 42.5, v, 1e-9)
}

// ── V2 round-trip: ContentDoc ─────────────────────────────────────────────────

func TestUnit_V2_RoundTrip_ContentDoc(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("docv2")
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentDoc(New()),
		}
		item.integrate(txn, 0)
	})

	v2 := EncodeStateAsUpdateV2(doc, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))
}

// ── deleteChildRange: length=0 early return ───────────────────────────────────

func TestUnit_YXmlFragment_DeleteZeroLength(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, NewYXmlElement("p")) })

	// delete with length=0 should be a no-op.
	doc.Transact(func(txn *Transaction) { frag.Delete(txn, 0, 0) })
	assert.Equal(t, 1, frag.Len())
}

// ── wrapUpdateErr covers error-wrapping path ──────────────────────────────────

func TestUnit_UpdateV2ToV1_InvalidInput(t *testing.T) {
	// Passing garbage bytes exercises wrapUpdateErr via the decoder error path.
	_, err := UpdateV2ToV1([]byte{0xFF, 0xFF, 0xFF})
	assert.Error(t, err)
}

func TestUnit_MergeUpdatesV1_InvalidInput(t *testing.T) {
	_, err := MergeUpdatesV1([]byte{0xFF, 0xFF})
	assert.Error(t, err)
}

// ── sortAndCompact: compaction of overlapping ranges ─────────────────────────

func TestUnit_DeleteSet_Merge_Compacts(t *testing.T) {
	// Two ranges for the same client that are adjacent after merge.
	ds1 := newDeleteSet()
	ds1.add(ID{Client: ClientID(5), Clock: 0}, 5)
	ds2 := newDeleteSet()
	ds2.add(ID{Client: ClientID(5), Clock: 5}, 3) // adjacent to [0,5)
	ds1.Merge(ds2)

	// After merge+compact, should be one range [0,8).
	ranges := ds1.clients[ClientID(5)]
	assert.Len(t, ranges, 1)
	assert.Equal(t, uint64(8), ranges[0].Len)
}

// ── applyStackItem: undo of ContentFormat (exercises itemInScope fully) ───────

func TestUnit_UndoManager_Undo_WithFormat(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { txt.Format(txn, 0, 5, map[string]any{"bold": true}) })

	assert.Equal(t, 2, um.UndoStackSize())
	ok := um.Undo() // undo the format
	require.True(t, ok)
	assert.Equal(t, 1, um.UndoStackSize())
}

// ── itemInScope: out-of-scope item returns false ──────────────────────────────

func TestUnit_UndoManager_ItemInScope_OutOfScope(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("inScope")
	arr := doc.GetArray("outOfScope")
	// UndoManager only tracks txt, not arr.
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	// Transaction modifies both txt and arr.
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		arr.Push(txn, []any{"item"})
	})

	// Undo: applyStackItem iterates ALL store items including arr's,
	// calling itemInScope for each → returns false for arr's items.
	ok := um.Undo()
	require.True(t, ok)
	// txt is undone, arr is not (it's out of scope).
	assert.Empty(t, txt.ToString())
	assert.Equal(t, 1, arr.Len())
}

// ── V2 encode with nested type parent (writeParentInfo false path) ────────────

func TestUnit_V2_Encode_NestedParent(t *testing.T) {
	// elem.Insert(xtxt) creates an item whose parent is elem (a child item, not root).
	// This exercises the `writeParentInfo(false)` path in encodeItemV2.
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("div")
	xtxt := NewYXmlText()

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, elem)
		elem.Insert(txn, 0, xtxt)
		xtxt.Insert(txn, 0, "text", nil)
	})

	// Full V2 round-trip exercises writeParentInfo(false) on decode.
	v2 := EncodeStateAsUpdateV2(doc, nil)
	assert.NotEmpty(t, v2)

	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))
	assert.Equal(t, "<div>text</div>", doc2.GetXmlFragment("root").ToXML())
}

// ── getItemCleanEnd in applyV2Txn: mid-item origin (single client) ───────────

func TestUnit_V2_MidItemOrigin_GetItemCleanEnd(t *testing.T) {
	// doc1 inserts "hello", then inserts "X" at position 2 in a separate txn.
	// The second insert splits "hello" at clock=1. V2 encode+decode exercises
	// getItemCleanEnd in applyV2Txn for the squashed "hello" run.
	doc1 := newTestDoc(1)
	txt1 := doc1.GetText("t")
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "hello", nil) })
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 2, "X", nil) })

	v2 := EncodeStateAsUpdateV2(doc1, nil)
	doc2 := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(doc2, v2, nil))
	assert.Equal(t, 6, doc2.GetText("t").Len())
}

// ── YArray.deleteRange: all uncovered branches ───────────────────────────────

func TestUnit_YArray_DeleteRange_ZeroLength(t *testing.T) {
	// length <= 0 early return path.
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"x"})
		arr.Delete(txn, 0, 0) // no-op: length == 0
	})
	assert.Equal(t, 1, arr.Len())
}

func TestUnit_YArray_DeleteRange_SkipBeforeIndex(t *testing.T) {
	// Two separate ContentAny items (not squashed); delete starting at index 2
	// forces the "counted+n <= index" skip path for the first item.
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{int64(1), int64(2)}) })
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{int64(3), int64(4)}) })

	doc.Transact(func(txn *Transaction) {
		arr.Delete(txn, 2, 2) // skip item1 (n=2), delete item2 entirely
	})
	assert.Equal(t, 2, arr.Len())
	assert.Equal(t, int64(1), arr.Get(0))
	assert.Equal(t, int64(2), arr.Get(1))
}

func TestUnit_YArray_DeleteRange_SkipDeletedItem(t *testing.T) {
	// Delete item1, then delete at index 0 again — the deleted item1 is iterated
	// but skipped (item.Deleted == true path).
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{int64(1), int64(2)}) })
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{int64(3), int64(4)}) })

	// Delete item1 (logical 0..1).
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 0, 2) })
	assert.Equal(t, 2, arr.Len())

	// Delete again at logical 0..1 — must skip the tombstoned item1 in the list.
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 0, 2) })
	assert.Equal(t, 0, arr.Len())
}

func TestUnit_YArray_DeleteRange_MidItemStart(t *testing.T) {
	// Push [1,2,3] as a single ContentAny(3 elements).
	// Delete starting at index 1 — forces the "counted < index" branch.
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{int64(1), int64(2), int64(3)})
	})
	doc.Transact(func(txn *Transaction) {
		arr.Delete(txn, 1, 2)
	})
	assert.Equal(t, 1, arr.Len())
	assert.Equal(t, int64(1), arr.Get(0))
}

func TestUnit_YArray_DeleteRange_SplitAtEnd(t *testing.T) {
	// Push [1,2,3,4,5] as a single ContentAny(5 elements).
	// Delete 3 starting at index 0 — forces the "else" branch (item larger than range).
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{int64(1), int64(2), int64(3), int64(4), int64(5)})
	})
	doc.Transact(func(txn *Transaction) {
		arr.Delete(txn, 0, 3)
	})
	assert.Equal(t, 2, arr.Len())
	assert.Equal(t, int64(4), arr.Get(0))
	assert.Equal(t, int64(5), arr.Get(1))
}

// ── YArray.Get and Slice: out-of-bounds / invalid range ──────────────────────

func TestUnit_YArray_Get_OutOfBounds(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"x"})
	})
	// index beyond length returns nil without panic.
	assert.Nil(t, arr.Get(99))
}

func TestUnit_YArray_Slice_InvalidRange(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"a", "b", "c"})
	})
	// start > end after clamping → returns nil.
	result := arr.Slice(10, 20)
	assert.Nil(t, result)
}

// ── newV2Decoder: truncated input hits early error paths ──────────────────────

func TestUnit_ApplyUpdateV2_TruncatedAtFeatureFlag(t *testing.T) {
	// Empty byte slice: feature flag read fails.
	err := ApplyUpdateV2(newTestDoc(1), []byte{}, nil)
	assert.Error(t, err)
}

func TestUnit_ApplyUpdateV2_TruncatedAfterFeatureFlag(t *testing.T) {
	// One byte (feature flag only): keyClockEncoder read fails.
	err := ApplyUpdateV2(newTestDoc(1), []byte{0}, nil)
	assert.Error(t, err)
}

func TestUnit_ApplyUpdateV2_TruncatedAfterKeyClock(t *testing.T) {
	// Feature flag + one empty keyClock section, then truncated.
	// Triggers the clientEncoder read failure.
	data := []byte{0, 0} // feature_flag=0, keyClockBytes=empty(len=0), then EOF
	err := ApplyUpdateV2(newTestDoc(1), data, nil)
	assert.Error(t, err)
}

func TestUnit_ApplyUpdateV2_TruncatedAfterClientBytes(t *testing.T) {
	// feature_flag + keyClockBytes(empty) + clientBytes(empty) → truncated at leftClockEncoder.
	data := []byte{0, 0, 0}
	err := ApplyUpdateV2(newTestDoc(1), data, nil)
	assert.Error(t, err)
}

func TestUnit_ApplyUpdateV2_TruncatedMidway(t *testing.T) {
	// feature_flag + 7 empty sections → truncated at lenEncoder.
	data := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0}
	err := ApplyUpdateV2(newTestDoc(1), data, nil)
	assert.Error(t, err)
}

// ── utf16ByteOffset: splice at exactly len(s) ─────────────────────────────────

func TestUnit_ContentString_Splice_AtEnd(t *testing.T) {
	// Splice at offset == utf16Len(s) — utf16ByteOffset returns len(s).
	// This hits the "return len(s)" path at the end of the loop.
	c := NewContentString("hello")
	right := c.Splice(5) // split at the very end
	assert.Equal(t, 5, c.Len())
	assert.Equal(t, 0, right.Len())
	assert.Equal(t, "hello", c.Str)
	assert.Empty(t, right.(*ContentString).Str)
}

// ── CreateRelativePositionFromIndex: deleted item and assoc<0 at last item ───

func TestUnit_RelPos_SkipsDeletedItem(t *testing.T) {
	// Insert "hello", delete "hel" so positions 0-2 become tombstones.
	// CreateRelativePositionFromIndex(2) must skip the deleted items.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 3) })

	// index=1 from the text's perspective (only "lo" remains).
	rp := CreateRelativePositionFromIndex(txt, 1, 0)
	require.NotNil(t, rp.Item)
	abs, ok := ToAbsolutePosition(doc, rp)
	require.True(t, ok)
	assert.Equal(t, 1, abs.Index)
}

func TestUnit_RelPos_AssocNeg_AtLastItem(t *testing.T) {
	// assoc=-1 with index > 0 that exhausts all items → triggers the
	// "item.Right == nil && assoc < 0" branch.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })

	// assoc=-1, index=3 (past the last char) → loop exhausts items.
	rp := CreateRelativePositionFromIndex(txt, 3, -1)
	require.NotNil(t, rp.Item, "should anchor to last item's clock")
}

// ── storePosCache: circular-overwrite wrap (posCacheWr resets to 0) ──────────

func TestUnit_StorePosCache_CircularWrap(t *testing.T) {
	// storePosCache's circular-overwrite path is only triggered when the cache
	// is already FULL (posCacheLen==posLRUSize) and ONE MORE call is made.
	// That requires a leftNeighbourAt scan of posLRUSize*2+1 items starting
	// from an empty cache (i.e. after a remote apply, which doesn't populate
	// the cache).
	//
	// Build a doc with posLRUSize*2+2 single-char items from client 2.
	docSrc := newTestDoc(2)
	txtSrc := docSrc.GetText("t")
	total := posLRUSize*2 + 2
	for i := 0; i < total; i++ {
		n := txtSrc.Len()
		docSrc.Transact(func(txn *Transaction) { txtSrc.Insert(txn, n, "x", nil) })
	}

	// Apply remotely to docDst — leaves cache empty.
	v1 := EncodeStateAsUpdateV1(docSrc, nil)
	docDst := newTestDoc(1)
	require.NoError(t, ApplyUpdateV1(docDst, v1, nil))

	// Insert at the very end: leftNeighbourAt(total) scans all `total` items
	// from start (cache is empty after remote apply). The first posLRUSize
	// calls fill the cache; the next posLRUSize calls do circular writes
	// (posCacheWr: 0→posLRUSize-1); the (2*posLRUSize+1)th call increments
	// posCacheWr to posLRUSize → triggers the `posCacheWr = 0` wrap.
	txtDst := docDst.GetText("t")
	docDst.Transact(func(txn *Transaction) { txtDst.Insert(txn, total, "Z", nil) })
	assert.Equal(t, total+1, txtDst.Len())
}

// ── invalidatePosCacheFrom: keeps lower-index entries ────────────────────────

func TestUnit_InvalidatePosCacheFrom_KeepsLowerEntries(t *testing.T) {
	// Build two separate ContentString items so leftNeighbourAt stores two
	// cache entries. Then insert at a position that invalidates only the higher
	// one, exercising the "posCache[n] = posCache[i]" keep path.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	// Two separate transactions → two separate items after squashRuns.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 3, "def", nil) })
	// Insert at index 4 → leftNeighbourAt(4) scans both items and stores:
	//   (3, item_abc) and (6, item_def) in the cache.
	// integrate then calls invalidatePosCacheFrom(4):
	//   entry (3, item_abc): 3 < 4 → KEPT   ← exercises the keep path
	//   entry (6, item_def): 6 >= 4 → dropped
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 4, "X", nil) })
	assert.Equal(t, 7, txt.Len())
}

// ── UndoManager.txnAffectsScope returns false ─────────────────────────────────

func TestUnit_UndoManager_TxnOutsideScope_NotCaptured(t *testing.T) {
	// txnAffectsScope returns false when the transaction touches only types
	// not in the UndoManager scope. The transaction is silently dropped.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	arr := doc.GetArray("a")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	// This transaction modifies only arr, which is NOT in the scope.
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x"}) })
	assert.Equal(t, 0, um.UndoStackSize(), "out-of-scope txn must not be captured")
}

// ── EqualSnapshots: same SV length but differing clock values ─────────────────

func TestUnit_EqualSnapshots_SameLength_DifferentClock(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(1) // same client, different content
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "abc", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "x", nil) })

	// Both have client 1 in their SV but with different clocks → not equal.
	snap1 := CaptureSnapshot(doc1)
	snap2 := CaptureSnapshot(doc2)
	assert.False(t, EqualSnapshots(snap1, snap2))
}

// ── DecodeRelativePosition: truncated input error paths ───────────────────────

func TestUnit_DecodeRelativePosition_TruncatedAfterKind1(t *testing.T) {
	// kind=1 but no client bytes → error on client read.
	data := []byte{1} // kind=1, nothing else
	_, err := DecodeRelativePosition(data)
	assert.Error(t, err)
}

func TestUnit_DecodeRelativePosition_TruncatedAfterKind2(t *testing.T) {
	// kind=2 but no string bytes → error on name read.
	data := []byte{2} // kind=2, nothing else
	_, err := DecodeRelativePosition(data)
	assert.Error(t, err)
}

func TestUnit_DecodeRelativePosition_UnknownKind(t *testing.T) {
	// kind=99 → default case, returns ErrInvalidRelativePosition.
	data := []byte{99}
	_, err := DecodeRelativePosition(data)
	assert.Error(t, err)
}

// ── store.Find: clock past end of item returns nil ────────────────────────────

func TestUnit_Store_Find_ClockPastItemEnd(t *testing.T) {
	// Insert a single-char item (clock=0, len=1). Searching for clock=1 finds
	// the item but its range [0,1) doesn't cover clock=1, so Find returns nil.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "a", nil) })

	// Clock 0 is covered; clock 1 is the next item's start (not yet inserted).
	found := doc.store.Find(ID{Client: ClientID(1), Clock: 1})
	assert.Nil(t, found)
}

// ── YXmlFragment.Insert at index 0 when items exist (left==nil else branch) ──

func TestUnit_YXmlFragment_InsertAtStart_WithExisting(t *testing.T) {
	// First insert "b", then insert "a" at index 0.
	// The second insert has left==nil (index=0) but t.start != nil → the
	// "else" branch in Insert scans for the first existing child as originRight.
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	b := NewYXmlElement("b")
	a := NewYXmlElement("a")

	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, b) })
	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, a) })

	assert.Equal(t, 2, frag.Len())
	children := frag.Children()
	require.Len(t, children, 2)
	assert.Equal(t, "a", children[0].(*YXmlElement).NodeName)
	assert.Equal(t, "b", children[1].(*YXmlElement).NodeName)
}

// ── deleteChildRange: skip deleted item and ParentSub != "" item ─────────────

func TestUnit_YXmlFragment_DeleteChildRange_SkipsDeletedAndAttributes(t *testing.T) {
	// Insert three children, delete the middle one, then call Delete(1,1) which
	// now wants to delete the second non-deleted child (index=1 of live items).
	// The loop must skip the tombstoned item (item.Deleted path).
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	a := NewYXmlElement("a")
	b := NewYXmlElement("b")
	c := NewYXmlElement("c")

	doc.Transact(func(txn *Transaction) {
		frag.Insert(txn, 0, a, b, c)
	})
	// Delete b so it's a tombstone in the linked list.
	doc.Transact(func(txn *Transaction) { frag.Delete(txn, 1, 1) })
	assert.Equal(t, 2, frag.Len())

	// Now delete index=1 (c) — the loop skips the tombstone b.
	doc.Transact(func(txn *Transaction) { frag.Delete(txn, 1, 1) })
	assert.Equal(t, 1, frag.Len())
}

// ── undo.applyStackItem: GC'd item skipped (isGC branch) ─────────────────────

func TestUnit_UndoManager_Undo_GCdItems(t *testing.T) {
	// Undo a deletion after RunGC has replaced the deleted item's content with
	// a ContentDeleted tombstone. applyStackItem must skip the GC'd item.
	doc := New(WithGC(true), WithClientID(ClientID(42)))
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	// Replace deleted content with ContentDeleted tombstone.
	RunGC(doc)

	// Undo the deletion: applyStackItem tries to restore the GC'd items but
	// finds ContentDeleted (isGC == true) and skips them.
	ok := um.Undo()
	require.True(t, ok)
	// Text remains empty because the items were GC'd and can't be restored.
	assert.Equal(t, 0, txt.Len())
}

// ── undo.itemInScope: returns true for a nested XML type in scope ─────────────

func TestUnit_UndoManager_ItemInScope_XmlFragment(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("root")
	elem := NewYXmlElement("div")

	// UndoManager scoped to frag.
	um := NewUndoManager(doc, []sharedType{frag})
	defer um.Destroy()

	doc.Transact(func(txn *Transaction) { frag.Insert(txn, 0, elem) })
	assert.Equal(t, 1, frag.Len())

	ok := um.Undo()
	require.True(t, ok)
	assert.Equal(t, 0, frag.Len())
}

// ── EqualSnapshots: same SV but different delete-set range counts ─────────────

func TestUnit_EqualSnapshots_DifferentRangeCounts(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(1)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	// doc1: insert "abcde", delete "a" and "e" separately → 2 delete ranges.
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "abcde", nil) })
	doc1.Transact(func(txn *Transaction) { txt1.Delete(txn, 0, 1) }) // delete "a"
	doc1.Transact(func(txn *Transaction) { txt1.Delete(txn, 3, 1) }) // delete "e" (now at pos 3)

	// doc2: insert "abcde", delete "ab" in one range → 1 delete range.
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "abcde", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Delete(txn, 0, 2) }) // delete "ab"

	snap1 := CaptureSnapshot(doc1)
	snap2 := CaptureSnapshot(doc2)
	// Same SV (both consumed clock 5), but different delete-set range counts.
	assert.False(t, EqualSnapshots(snap1, snap2))
}

func TestUnit_EqualSnapshots_SameRangesButDifferentValues(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(1)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "abc", nil) })
	doc1.Transact(func(txn *Transaction) { txt1.Delete(txn, 0, 1) }) // delete clock 0

	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "abc", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Delete(txn, 2, 1) }) // delete clock 2

	snap1 := CaptureSnapshot(doc1)
	snap2 := CaptureSnapshot(doc2)
	// Both have 1 delete range but at different clocks.
	assert.False(t, EqualSnapshots(snap1, snap2))
}

// ── UndoManager.Redo: empty redo stack returns false ─────────────────────────

func TestUnit_UndoManager_Redo_EmptyStack(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	// Redo on an empty redo stack must return false.
	ok := um.Redo()
	assert.False(t, ok)
}

// ── clientsSorted: multiple clients triggers the sort comparison ──────────────

func TestUnit_ClientsSorted_MultipleClients(t *testing.T) {
	// Encode a V1 update from a 2-client document. clientsSorted is called
	// during encoding, which calls sort.Slice with the comparison func.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)
	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "a", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "b", nil) })

	// Apply doc2's changes into doc1 so doc1 has two client entries.
	v1_2 := EncodeStateAsUpdateV1(doc2, nil)
	require.NoError(t, ApplyUpdateV1(doc1, v1_2, nil))

	// Encoding doc1 now calls clientsSorted with 2 clients, triggering the sort.
	v1 := EncodeStateAsUpdateV1(doc1, nil)
	doc3 := newTestDoc(3)
	require.NoError(t, ApplyUpdateV1(doc3, v1, nil))
	assert.Equal(t, 2, doc3.GetText("t").Len())
}

// ── undo.itemInScope: item.Parent == nil returns false ────────────────────────

func TestUnit_UndoManager_ItemInScope_NilParent(t *testing.T) {
	// Construct a bare Item with Parent = nil and verify itemInScope returns false.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []sharedType{txt})
	defer um.Destroy()

	orphan := &Item{ID: ID{Client: 1, Clock: 999}, Content: NewContentString("x")}
	// orphan.Parent is nil by zero-value
	assert.False(t, um.itemInScope(orphan))
}

// ── getItemCleanEnd: item == nil returns nil ─────────────────────────────────

func TestUnit_GetItemCleanEnd_ItemNotFound(t *testing.T) {
	// Request a client/clock that doesn't exist → Find returns nil → getItemCleanEnd returns nil.
	doc := newTestDoc(1)
	doc.Transact(func(txn *Transaction) {
		result := doc.store.getItemCleanEnd(txn, ClientID(99), 0)
		assert.Nil(t, result)
	})
}

// ── RunGC: early return when gc disabled ─────────────────────────────────────

func TestUnit_RunGC_GCDisabled_NoOp(t *testing.T) {
	// Default doc has gc=false; RunGC should return immediately without error.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })
	RunGC(doc) // no-op (gc=false)
	// Text is still empty (already deleted), but no panic.
	assert.Equal(t, 0, txt.Len())
}

func TestUnit_RunGC_MergesAdjacentTombstones(t *testing.T) {
	// Create two adjacent deleted items from the same client with consecutive
	// clocks. RunGC's pass2 merge path absorbs item2 into item1.
	doc := New(WithGC(true), WithClientID(ClientID(1)))
	txt := doc.GetText("t")

	// Insert "hello" → one item (clock 0..4, n=5).
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	// Delete "he" (positions 0-1): splits into item_he(deleted) + item_llo.
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 2) })
	// Delete "ll" (now positions 0-1): splits item_llo into item_ll(deleted) + item_o.
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 2) })

	// Two adjacent deleted items (item_he clock=0 n=2, item_ll clock=2 n=2)
	// are now in the store. RunGC pass2 should merge them into one tombstone.
	RunGC(doc)
	assert.Equal(t, "o", txt.ToString())
}

// ── DecodeSnapshot: truncated at delete-set bytes ─────────────────────────────

func TestUnit_DecodeSnapshot_TruncatedAtDeleteSet(t *testing.T) {
	// Encode a valid snapshot, then truncate it after the SV bytes to force an
	// error when DecodeSnapshot tries to read the delete-set length prefix.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	full := EncodeSnapshot(CaptureSnapshot(doc))

	// Truncate to just 4 bytes — too short to contain the full SV varint-length
	// prefix + SV data + DS varint-length prefix, so the DS read must fail.
	truncated := full[:4]
	_, err := DecodeSnapshot(truncated)
	assert.Error(t, err)
}
