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
//   - if caller passed explicit attrs, opens markers for the diff, inserts,
//     then emits negating markers to revert to currentAttributes after
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

// Regression for the `anchor == nil` early-exit in currentAttributesAt
// (Copilot review comment #1). Without the early-exit, the walk runs to
// the END of the document and returns end-state attrs — so an Insert at
// position 0 with explicit attrs would compute a diff against the wrong
// baseline (the doc's later state instead of the empty start state),
// potentially skipping marker emission.
func TestUnit_YText_Insert_AtStart_ExplicitAttrs_DoesNotInheritFromEndOfDoc(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	// Build a doc with bold formatting LATER in the text.
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "later", Attributes{"bold": true})
	})

	// Now Insert AT POSITION 0 (before the bold span) with explicit
	// {bold: true}. The currentAttributes anchor here is nil — the cursor is
	// before any item. With the early-exit, currentAttrs = {} (start state).
	// effective {bold: true} vs current {} → diff has bold:true → emit
	// opener + closer for the new insert.
	//
	// Without the early-exit, currentAttributesAt(nil) would walk the entire
	// doc and return the end-state attrs (which happen to be {} here because
	// the bold span has a closer, but in a more complex doc the bug would
	// produce wrong results). We assert the correct behavior via marker
	// count: the new insert MUST contribute its own opener+closer pair, not
	// rely on a phantom-shared opener with the later span.
	beforeMarkers := countLiveContentFormat(doc)
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "X", Attributes{"bold": true})
	})
	assert.Equal(t, beforeMarkers+2, countLiveContentFormat(doc),
		"Insert(0, ..., bold:true) must emit its own opener+closer pair "+
			"independent of formatting later in the doc (anchor==nil early-exit)")

	// Sanity: the inserted X must actually be bold in ToDelta.
	delta := txt.ToDelta()
	require.NotEmpty(t, delta)
	first := delta[0]
	require.Equal(t, "X", first.Insert)
	assert.Equal(t, Attributes{"bold": true}, first.Attributes,
		"X must be bold via its OWN marker pair, not by accident")
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
