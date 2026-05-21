package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the YText overhaul covering A1 (format-marker overlap cleanup),
// A4 (cleanup of dangling format markers on delete), and #76 (InsertEmbed).
// A2 and A3 (Insert with currentAttributes diff) are scoped to a follow-up PR.

// countLiveContentFormat returns how many ContentFormat items exist in the
// store that are not tombstoned. Used to detect orphaned format markers
// after delete operations (#71 vector A4).
func countLiveContentFormat(doc *Doc) int {
	n := 0
	for _, items := range doc.store.clients {
		for _, item := range items {
			if _, ok := item.Content.(*ContentFormat); ok && !item.Deleted {
				n++
			}
		}
	}
	return n
}

// A1 (HIGH) — YText.Format must clean up overlapping same-key markers within
// the target range. Otherwise repeated toggling of the same attribute leaves
// dead opening/closing pairs that bloat the document and can confuse readers.
// Yjs JS YText.formatText walks the range and deletes pre-existing
// ContentFormat items for the same key.
func TestUnit_YText_Format_OverlappingSameKey_DoesNotAccumulate(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		txt.Format(txn, 0, 5, Attributes{"bold": true})
		txt.Format(txn, 0, 5, Attributes{"bold": nil}) // unbold
	})

	delta := txt.ToDelta()
	require.Len(t, delta, 1,
		"unbold-after-bold must produce a single plain Delta, not fragments")
	assert.Equal(t, "hello", delta[0].Insert)
	assert.Empty(t, delta[0].Attributes,
		"unbold must leave no attribute residue in ToDelta output")
}

// A1 cont. — toggling bold on/off multiple times must NOT accumulate markers.
// Pre-fix every toggle adds two more markers; post-fix Format walks the range
// and removes prior same-key markers before inserting new ones.
func TestUnit_YText_Format_RepeatedToggles_DoNotInflateStore(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
	})

	// One bold + unbold = baseline marker count (we measure after one cycle).
	doc.Transact(func(txn *Transaction) {
		txt.Format(txn, 0, 5, Attributes{"bold": true})
		txt.Format(txn, 0, 5, Attributes{"bold": nil})
	})
	baseline := countLiveContentFormat(doc)

	// Three more toggle cycles. With cleanup, marker count stays bounded; pre-fix
	// it would grow by 4 per cycle (open+close on bold-true, open on bold-nil,
	// no close on bold-nil).
	for i := 0; i < 3; i++ {
		doc.Transact(func(txn *Transaction) {
			txt.Format(txn, 0, 5, Attributes{"bold": true})
			txt.Format(txn, 0, 5, Attributes{"bold": nil})
		})
	}
	after := countLiveContentFormat(doc)

	assert.LessOrEqual(t, after-baseline, 2,
		"repeated toggle must not accumulate more than a small constant "+
			"of markers: baseline=%d after=%d", baseline, after)
}

// A4 (MEDIUM) — Delete must tombstone ContentFormat markers that no longer
// wrap any live content. Yjs JS deleteText calls cleanupFormattingGap which
// walks the gap and removes orphan markers. Pre-fix ygo leaves the markers
// as live items in the store, bloating the document over time.
func TestUnit_YText_Delete_CleansUpDanglingFormatMarkers(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		txt.Format(txn, 0, 5, Attributes{"bold": true})
	})

	require.Greater(t, countLiveContentFormat(doc), 0,
		"format markers exist after Format()")

	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 0, txt.Len())
	})

	assert.Equal(t, 0, countLiveContentFormat(doc),
		"format markers must be tombstoned when their wrapped content is fully deleted")
}

// A4 cont. — deleting all content inside a Format span tombstones both the
// opener and the closer (their effect zone is empty). Uses Format (not
// Insert-with-attrs) because Format inserts both opener AND closer; the
// Insert-with-attrs case requires the A3 fix that's deferred to a follow-up PR.
func TestUnit_YText_Delete_WrappedSpan_TombstonesBothMarkers(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "abc DEF ghi", nil)
		txt.Format(txn, 4, 3, Attributes{"bold": true}) // bold "DEF"
	})
	require.Greater(t, countLiveContentFormat(doc), 0,
		"setup: bold markers must exist around DEF")

	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 4, 3) // delete bold "DEF"
	})

	assert.Equal(t, "abc  ghi", txt.ToString())
	assert.Equal(t, 0, countLiveContentFormat(doc),
		"both opener and closer must be tombstoned when their wrapped content is gone")
}

// A4 cont. — partial deletion leaves SOME live content in the format span,
// so the surrounding markers must NOT be tombstoned (they still wrap live
// content and affect ToDelta semantics).
func TestUnit_YText_Delete_PartialDelete_KeepsMarkers(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "abc DEF ghi", nil)
		txt.Format(txn, 4, 3, Attributes{"bold": true}) // bold "DEF"
	})
	before := countLiveContentFormat(doc)

	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 4, 1) // delete only "D" of "DEF" — "EF" still bold
	})

	assert.Equal(t, before, countLiveContentFormat(doc),
		"partial deletion leaves live content in the span; markers must stay")
}

// #76 (HIGH) — YText.InsertEmbed adds an embedded object to the rich-text
// stream. Wire format already supports ContentEmbed; ygo was missing the
// public API to insert one. Without it, callers couldn't represent images,
// formulas, or other inline embeds at all.
func TestUnit_YText_InsertEmbed_Basic(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	embed := map[string]any{"image": "https://example.com/x.png"}

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "before", nil)
		txt.InsertEmbed(txn, 6, embed, nil)
		txt.Insert(txn, 7, "after", nil)
	})

	// Each embed counts as one UTF-16 code unit in length (Yjs convention).
	assert.Equal(t, 12, txt.Len(),
		"len = 6 (before) + 1 (embed) + 5 (after) = 12")

	delta := txt.ToDelta()
	require.Len(t, delta, 3,
		"three delta ops: text before, embed, text after")
	assert.Equal(t, "before", delta[0].Insert)
	assert.Equal(t, embed, delta[1].Insert,
		"embed Delta carries the embed value, not a string")
	assert.Equal(t, "after", delta[2].Insert)
}

// #76 cont. — embed at the beginning of an empty doc.
func TestUnit_YText_InsertEmbed_AtStart(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	embed := map[string]any{"formula": "x^2"}

	doc.Transact(func(txn *Transaction) {
		txt.InsertEmbed(txn, 0, embed, nil)
	})

	assert.Equal(t, 1, txt.Len())
	delta := txt.ToDelta()
	require.Len(t, delta, 1)
	assert.Equal(t, embed, delta[0].Insert)
}

// #76 cont. — embed with attributes wraps the embed in opening + closing
// format markers, just like text. Pre-fix this would have panicked because
// InsertEmbed didn't exist.
func TestUnit_YText_InsertEmbed_WithAttributes(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	embed := map[string]any{"image": "x.png"}

	doc.Transact(func(txn *Transaction) {
		txt.InsertEmbed(txn, 0, embed, Attributes{"alt": "An image"})
	})

	delta := txt.ToDelta()
	require.Len(t, delta, 1)
	assert.Equal(t, embed, delta[0].Insert)
	assert.Equal(t, Attributes{"alt": "An image"}, delta[0].Attributes,
		"attrs on InsertEmbed must attach to the embed Delta")
}

// #76 cont. — cross-peer convergence on embeds. docA inserts an embed; docB
// must see it after sync.
func TestInteg_YText_InsertEmbed_CrossPeer(t *testing.T) {
	docA := newTestDoc(1)
	txtA := docA.GetText("t")
	embed := map[string]any{"video": "y.mp4"}

	docA.Transact(func(txn *Transaction) {
		txtA.Insert(txn, 0, "watch: ", nil)
		txtA.InsertEmbed(txn, 7, embed, nil)
	})

	docB := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, nil), nil))
	txtB := docB.GetText("t")

	deltaB := txtB.ToDelta()
	require.Len(t, deltaB, 2)
	assert.Equal(t, "watch: ", deltaB[0].Insert)
	assert.Equal(t, embed, deltaB[1].Insert)
}
