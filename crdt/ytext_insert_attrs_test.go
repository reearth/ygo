package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for issue #71 vectors A2 + A3 — YText.Insert with currentAttributes
// diff and negating markers after the insert. Companion to PR #85 which
// addressed A1 (Format overlap cleanup) and A4 (cleanupFormattingGap on
// Delete).
//
// Yjs JS's insertText:
//   - computes currentAttributes by walking left from the cursor
//   - if caller passed nil/empty attrs, the new text inherits currentAttributes
//   - if caller passed explicit attrs, clears every current attribute attrs
//     does not name, opens markers for the diff, inserts, then emits negating
//     markers to revert to currentAttributes after
//
// Pre-fix ygo Insert only emitted opening markers when attrs was non-empty,
// and never emitted closing/negating markers — so formatting bled rightward.

// A3 — Insert with explicit attrs must emit BOTH opening AND negating
// closing markers around the inserted text. Without the closing markers,
// formatting bleeds through subsequent retained text. Pre-fix Insert emits
// only the opener; post-fix it emits both, matching Yjs JS.
func TestUnit_YText_Insert_WithAttrs_EmitsClosingMarkers(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "X", Attributes{"bold": true})
	})

	n := countLiveContentFormat(doc)
	assert.Equal(t, 2, n,
		"Insert with attrs must emit BOTH an opener and a negating closer "+
			"(#71 A3); pre-fix only the opener was emitted")
}

// A3 — Multiple attrs each get an opener + closer pair.
func TestUnit_YText_Insert_WithMultipleAttrs_EmitsClosersForEach(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "X", Attributes{"bold": true, "italic": true})
	})

	n := countLiveContentFormat(doc)
	assert.Equal(t, 4, n,
		"two attrs must emit two openers + two negating closers (4 markers total)")
}

// A3 — Insert with nil attrs in a doc with no formatting emits no markers.
// The fast path: empty currentAttributes + nil caller attrs = empty diff = no work.
func TestUnit_YText_Insert_NilAttrs_PlainContext_EmitsNoMarkers(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
	})

	assert.Equal(t, 0, countLiveContentFormat(doc),
		"plain Insert into plain context must emit no format markers")
}

// A2 — Insert with nil attrs at the END of a bold span (inside the bold
// region, before its closing marker) inherits bold. The cursor is between
// the bold text item and the closer, so currentAttributes there is {bold:true};
// nil caller attrs means "use whatever currentAttributes says" — the new text
// is bold WITHOUT requiring the caller to pass attrs explicitly.
func TestUnit_YText_Insert_NilAttrs_InsideBoldSpan_Inherits(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "abc", Attributes{"bold": true})
		txt.Insert(txn, 3, "def", nil) // nil → should inherit bold
	})

	delta := txt.ToDelta()
	// ToDelta emits one Delta per ContentString item; it doesn't merge
	// adjacent same-attribute runs. The inheritance contract is verified
	// by checking that EVERY non-empty insert in the result carries the
	// inherited attribute.
	require.NotEmpty(t, delta)
	var joined string
	for _, d := range delta {
		s, ok := d.Insert.(string)
		require.True(t, ok, "all entries are string inserts in this test")
		joined += s
		assert.Equal(t, Attributes{"bold": true}, d.Attributes,
			"every Delta entry must carry the inherited bold attribute (#71 A2)")
	}
	assert.Equal(t, "abcdef", joined,
		"the two inserts must together produce the expected content")
}

// A2 + A3 together — three inserts: bold first, then nil-attrs at end
// (inherits), then nil-attrs at start (should NOT inherit because the
// cursor is before any opening marker, currentAttributes is empty).
func TestUnit_YText_Insert_NilAttrs_OutsideBoldSpan_DoesNotInherit(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "bold", Attributes{"bold": true})
	})
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "plain-", nil) // BEFORE the opener
	})

	delta := txt.ToDelta()
	require.Len(t, delta, 2)
	assert.Equal(t, "plain-", delta[0].Insert)
	assert.Empty(t, delta[0].Attributes,
		"insert before the opening marker stays plain — currentAttributes is empty there")
	assert.Equal(t, "bold", delta[1].Insert)
	assert.Equal(t, Attributes{"bold": true}, delta[1].Attributes)
}

// A2 + A3 together — explicit caller attrs that already match
// currentAttributes produce no extra markers (the diff is empty).
func TestUnit_YText_Insert_ExplicitAttrsMatchingContext_NoExtraMarkers(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "abc", Attributes{"bold": true})
	})
	beforeMarkers := countLiveContentFormat(doc)
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 3, "def", Attributes{"bold": true}) // already bold here
	})
	afterMarkers := countLiveContentFormat(doc)

	assert.Equal(t, beforeMarkers, afterMarkers,
		"explicit attrs that match currentAttributes must not produce new markers")
}

// Regression — non-comparable attribute values (slices, maps) from
// JSON-decoded ContentFormat must not panic during the diff calculation.
// Pre-fix, `oldVal == newVal` would panic at runtime when comparing
// `[]any` / `map[string]any` values. Now uses reflect.DeepEqual.
func TestUnit_YText_Insert_NonComparableAttrValue_DoesNotPanic(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	complexAttr := Attributes{
		"link":   []any{"https://example.com", "title"},
		"meta":   map[string]any{"weight": float64(700)},
		"simple": "value",
	}
	assert.NotPanics(t, func() {
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, 0, "hello", complexAttr)
			// Second insert with identical complex attrs — exercises the
			// reflect.DeepEqual path (same value, no markers needed).
			txt.Insert(txn, 5, "world", complexAttr)
		})
	}, "Insert must handle non-comparable attr values via reflect.DeepEqual")

	// Sanity check that all three attrs round-trip.
	delta := txt.ToDelta()
	require.NotEmpty(t, delta)
	assert.Equal(t, complexAttr, delta[0].Attributes)
}

// Regression — when oldVal and newVal are both non-comparable but DIFFERENT,
// reflect.DeepEqual must return false and the key must appear in the diff.
// Exercises the inequality branch of the DeepEqual replacement.
func TestUnit_YText_Insert_NonComparableAttrValue_DifferentValues_DiffsCorrectly(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "A", Attributes{"link": []any{"https://example.com/a"}})
	})
	beforeMarkers := countLiveContentFormat(doc)

	// Same key, DIFFERENT slice value → must be diffed (not skipped). Emits
	// a new opener carrying the new link.
	assert.NotPanics(t, func() {
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, 1, "B", Attributes{"link": []any{"https://example.com/b"}})
		})
	})

	// More markers added — the diff path was taken, not the same-skip path.
	assert.Greater(t, countLiveContentFormat(doc), beforeMarkers,
		"different non-comparable values must take the diff path and emit new markers")
}

// An Insert at position 0 with explicit attrs must use the start state, not
// the doc's later state. Before a matching opener it reuses that opener rather
// than emitting its own pair, as Yjs's minimizeAttributeChanges does.
func TestUnit_YText_Insert_AtStart_ExplicitAttrs_DoesNotInheritFromEndOfDoc(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "later", Attributes{"bold": true})
		txt.Insert(txn, 5, "plain", Attributes{"italic": true})
	})
	beforeMarkers := countLiveContentFormat(doc)
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "X", Attributes{"bold": true})
		txt.Insert(txn, 0, "Y", Attributes{"italic": true})
	})
	// X reuses "later"'s opener; Y needs its own pair.
	assert.Equal(t, beforeMarkers+2, countLiveContentFormat(doc))
	assert.Equal(t, []Delta{
		{Op: DeltaOpInsert, Insert: "Y", Attributes: Attributes{"italic": true}},
		{Op: DeltaOpInsert, Insert: "Xlater", Attributes: Attributes{"bold": true}},
		{Op: DeltaOpInsert, Insert: "plain", Attributes: Attributes{"italic": true}},
	}, txt.ToDelta())
}

// A3 — Cross-peer convergence: docB receives docA's Insert-with-attrs and
// must produce the same ToDelta output (including the bounded formatting).
func TestInteg_YText_Insert_WithAttrs_CrossPeerConvergence(t *testing.T) {
	docA := newTestDoc(1)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *Transaction) {
		txtA.Insert(txn, 0, "abc", Attributes{"bold": true})
		txtA.Insert(txn, 3, "def", nil) // inherits bold
		// Bold ends at position 6; nothing after.
	})

	docB := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, nil), nil))
	txtB := docB.GetText("t")

	deltaA := txtA.ToDelta()
	deltaB := txtB.ToDelta()
	assert.Equal(t, deltaA, deltaB,
		"docA and docB must produce identical ToDelta after sync")
}

// A formatted insert's markers and content all keep the cursor's right
// neighbour as originRight, as Yjs's insertText does.
func TestUnit_YText_FormattedInsert_KeepsRightOrigin(t *testing.T) {
	for name, insert := range map[string]func(*YText, *Transaction){
		"Insert":      func(txt *YText, txn *Transaction) { txt.Insert(txn, 1, "x", Attributes{"bold": true}) },
		"InsertEmbed": func(txt *YText, txn *Transaction) { txt.InsertEmbed(txn, 1, 7, Attributes{"bold": true}) },
		"ApplyDelta": func(txt *YText, txn *Transaction) {
			txt.ApplyDelta(txn, []Delta{{Op: DeltaOpRetain, Retain: 1}, {Op: DeltaOpInsert, Insert: "x", Attributes: Attributes{"bold": true}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := newTestDoc(1)
			st := src.GetText("t")
			src.Transact(func(txn *Transaction) { st.Insert(txn, 0, "ab", nil) })
			doc := newTestDoc(2)
			require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(src, nil), nil))
			txt := doc.GetText("t")
			doc.Transact(func(txn *Transaction) { insert(txt, txn) })
			b := ID{Client: 1, Clock: 1}
			items := doc.store.clients[2]
			require.Len(t, items, 3) // opener, content, closer
			for _, it := range items {
				require.NotNil(t, it.OriginRight, "%T", it.Content)
				assert.Equal(t, b, *it.OriginRight, "%T", it.Content)
			}
		})
	}
}

// An insert given attributes carries exactly those, clearing the ones around
// it, while Insert without attributes inherits them, as in Yjs's insertText.
func TestUnit_YText_InsertAttrs_ClearUnnamed(t *testing.T) {
	bold := Attributes{"bold": true}
	run := func(insert func(*YText, *Transaction)) []Delta {
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcd", bold) })
		doc.Transact(func(txn *Transaction) { insert(txt, txn) })
		return txt.ToDelta()
	}
	split := func(mid Delta) []Delta {
		return []Delta{{Op: DeltaOpInsert, Insert: "ab", Attributes: bold}, mid, {Op: DeltaOpInsert, Insert: "cd", Attributes: bold}}
	}
	for name, tc := range map[string]struct {
		insert func(*YText, *Transaction)
		want   []Delta
	}{
		"Insert/nil": {
			func(txt *YText, txn *Transaction) { txt.Insert(txn, 2, "x", nil) },
			[]Delta{{Op: DeltaOpInsert, Insert: "abxcd", Attributes: bold}},
		},
		"Insert/other": {
			func(txt *YText, txn *Transaction) { txt.Insert(txn, 2, "x", Attributes{"italic": true}) },
			split(Delta{Op: DeltaOpInsert, Insert: "x", Attributes: Attributes{"italic": true}}),
		},
		"InsertEmbed/nil": {
			func(txt *YText, txn *Transaction) { txt.InsertEmbed(txn, 2, 7, nil) },
			split(Delta{Op: DeltaOpInsert, Insert: int64(7)}),
		},
		"ApplyDelta/nil": {
			func(txt *YText, txn *Transaction) {
				txt.ApplyDelta(txn, []Delta{{Op: DeltaOpRetain, Retain: 2}, {Op: DeltaOpInsert, Insert: "x"}})
			},
			split(Delta{Op: DeltaOpInsert, Insert: "x"}),
		},
		"ApplyDelta/embed": {
			func(txt *YText, txn *Transaction) {
				txt.ApplyDelta(txn, []Delta{{Op: DeltaOpRetain, Retain: 2}, {Op: DeltaOpInsert, Insert: 7, Attributes: Attributes{"italic": true}}})
			},
			split(Delta{Op: DeltaOpInsert, Insert: int64(7), Attributes: Attributes{"italic": true}}),
		},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, tc.want, run(tc.insert)) })
	}
}
