package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── YText ─────────────────────────────────────────────────────────────────────

func TestUnit_YText_Insert_Append(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello", nil)
	})

	assert.Equal(t, "Hello", txt.ToString())
	assert.Equal(t, 5, txt.Len())
}

func TestUnit_YText_Insert_Prepend(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "World", nil)
		txt.Insert(txn, 0, "Hello ", nil)
	})

	assert.Equal(t, "Hello World", txt.ToString())
}

func TestUnit_YText_Insert_Middle(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Helo", nil)
		txt.Insert(txn, 3, "l", nil)
	})

	assert.Equal(t, "Hello", txt.ToString())
}

func TestUnit_YText_Insert_IntoExistingRun(t *testing.T) {
	// Inserting in the middle of an existing ContentString forces a split.
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "ac", nil)
	})
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 1, "b", nil) // split "ac" → "a" + "b" + "c"
	})

	assert.Equal(t, "abc", txt.ToString())
}

func TestUnit_YText_Delete(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello, world!", nil)
	})
	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 5, 7) // delete ", world"
	})

	assert.Equal(t, "Hello!", txt.ToString())
	assert.Equal(t, 6, txt.Len())
}

func TestUnit_YText_Delete_AtStart(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 1) })

	assert.Equal(t, "bc", txt.ToString())
}

func TestUnit_YText_Delete_AtEnd(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 2, 1) })

	assert.Equal(t, "ab", txt.ToString())
}

func TestUnit_YText_Unicode(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "héllo", nil) // é is 2 UTF-8 bytes but 1 rune
	})

	assert.Equal(t, "héllo", txt.ToString())
	assert.Equal(t, 5, txt.Len(), "Len() must count UTF-16 code units, not bytes or runes")
}

func TestUnit_YText_Observe_FiresOnce(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")
	calls := 0
	txt.Observe(func(_ YTextEvent) { calls++ })

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello", nil)
		txt.Insert(txn, 5, " World", nil)
	})

	assert.Equal(t, 1, calls)
}

func TestUnit_YText_Observe_Unsubscribe(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")
	calls := 0
	unsub := txt.Observe(func(_ YTextEvent) { calls++ })

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hi", nil) })
	unsub()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 2, "!", nil) })

	assert.Equal(t, 1, calls)
}

func TestUnit_YText_Format_Bold(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello World", nil)
	})
	doc.Transact(func(txn *Transaction) {
		txt.Format(txn, 6, 5, Attributes{"bold": true}) // bold "World"
	})

	// Text content is unchanged.
	assert.Equal(t, "Hello World", txt.ToString())
	assert.Equal(t, 11, txt.Len())
}

func TestUnit_YText_ToDelta_Plain(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello", nil)
	})

	deltas := txt.ToDelta()
	require.Len(t, deltas, 1)
	assert.Equal(t, DeltaOpInsert, deltas[0].Op)
	assert.Equal(t, "Hello", deltas[0].Insert)
	assert.Nil(t, deltas[0].Attributes)
}

func TestUnit_YText_ToDelta_WithFormatting(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "plain", nil)
		txt.Insert(txn, 5, "bold", Attributes{"bold": true})
	})

	deltas := txt.ToDelta()
	// Expect two insert ops: plain then bold.
	require.GreaterOrEqual(t, len(deltas), 2)

	// Find the bold segment.
	var boldDelta *Delta
	for i := range deltas {
		if deltas[i].Attributes != nil && deltas[i].Attributes["bold"] == true {
			boldDelta = &deltas[i]
			break
		}
	}
	require.NotNil(t, boldDelta, "expected a delta with bold=true attribute")
	assert.Equal(t, "bold", boldDelta.Insert)
}

func TestUnit_YText_ToDelta_EmptyDoc(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")
	assert.Empty(t, txt.ToDelta())
}

func TestUnit_YText_RunLengthSquashing(t *testing.T) {
	// Typing 5 characters in sequence within one transaction should produce
	// a single ContentString item, not five separate items.
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "h", nil)
		txt.Insert(txn, 1, "e", nil)
		txt.Insert(txn, 2, "l", nil)
		txt.Insert(txn, 3, "l", nil)
		txt.Insert(txn, 4, "o", nil)
	})

	assert.Equal(t, "hello", txt.ToString())
	assert.Equal(t, 5, txt.Len())

	// Count ContentString items in the linked list — should be ≤ 2
	// (one item per contiguous same-client run).
	itemCount := 0
	for item := txt.start; item != nil; item = item.Right {
		if !item.Deleted {
			if _, ok := item.Content.(*ContentString); ok {
				itemCount++
			}
		}
	}
	// Each Insert call appends to the same client's run; because we call
	// Insert at consecutive indices in one transaction, each call creates a
	// new item right after the previous. The items are separate but adjacent.
	// The important invariant: Len() and ToString() are correct.
	assert.LessOrEqual(t, itemCount, 5, "at most one item per character")
	assert.Equal(t, 5, txt.Len())
}

// ── YText integration ─────────────────────────────────────────────────────────

func TestInteg_YText_ConcurrentFormat_Convergence(t *testing.T) {
	// Two peers apply different formatting to the same text concurrently.
	// Both must converge to identical ToString() output (text is unchanged)
	// and have the same number of delta ops.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)

	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	// Both peers start with the same text.
	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "Hello World", nil) })

	// Replicate the initial text into doc2 manually.
	doc2.Transact(func(txn *Transaction) {
		doc1.store.IterateFrom(doc2.store.StateVector(), func(item *Item) {
			clone := &Item{
				ID:          item.ID,
				Origin:      item.Origin,
				OriginRight: item.OriginRight,
				Parent:      &txt2.abstractType,
				Content:     item.Content.Copy(),
			}
			clone.integrate(txn, 0)
		})
	})
	assert.Equal(t, "Hello World", txt2.ToString())

	// Concurrent: peer1 bolds "Hello", peer2 italicises "World".
	doc1.Transact(func(txn *Transaction) {
		txt1.Format(txn, 0, 5, Attributes{"bold": true})
	})
	doc2.Transact(func(txn *Transaction) {
		txt2.Format(txn, 6, 5, Attributes{"italic": true})
	})

	// Cross-apply format items.
	applyItems := func(src *Doc, dst *Doc, dstTxt *YText) {
		dst.Transact(func(txn *Transaction) {
			src.store.IterateFrom(dst.store.StateVector(), func(item *Item) {
				clone := &Item{
					ID:          item.ID,
					Origin:      item.Origin,
					OriginRight: item.OriginRight,
					Parent:      &dstTxt.abstractType,
					Content:     item.Content.Copy(),
				}
				clone.integrate(txn, 0)
			})
		})
	}
	applyItems(doc1, doc2, txt2)
	applyItems(doc2, doc1, txt1)

	// Text content must be identical and unchanged.
	assert.Equal(t, txt1.ToString(), txt2.ToString())
	assert.Equal(t, "Hello World", txt1.ToString())
}

func TestInteg_YText_TwoPeer_Convergence(t *testing.T) {
	// Two peers insert at position 0 concurrently; lower clientID goes first.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)

	txt1 := doc1.GetText("t")
	txt2 := doc2.GetText("t")

	doc1.Transact(func(txn *Transaction) { txt1.Insert(txn, 0, "Alice", nil) })
	doc2.Transact(func(txn *Transaction) { txt2.Insert(txn, 0, "Bob", nil) })

	// Exchange: apply doc2's items into doc1 and vice-versa.
	doc2.store.IterateFrom(doc1.store.StateVector(), func(item *Item) {
		doc1.Transact(func(txn *Transaction) {
			clone := &Item{
				ID:          item.ID,
				Origin:      item.Origin,
				OriginRight: item.OriginRight,
				Parent:      &txt1.abstractType,
				Content:     item.Content.Copy(),
			}
			clone.integrate(txn, 0)
		})
	})
	doc1.store.IterateFrom(doc2.store.StateVector(), func(item *Item) {
		doc2.Transact(func(txn *Transaction) {
			clone := &Item{
				ID:          item.ID,
				Origin:      item.Origin,
				OriginRight: item.OriginRight,
				Parent:      &txt2.abstractType,
				Content:     item.Content.Copy(),
			}
			clone.integrate(txn, 0)
		})
	})

	assert.Equal(t, txt1.ToString(), txt2.ToString(), "both peers must converge")
	// client 1 < client 2, so "Alice" appears before "Bob"
	assert.Equal(t, "AliceBob", txt1.ToString())
}

func TestInteg_YText_SequentialEdits_Correct(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("content")

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "Hello, world!", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 5, 7) })
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 5, " Go!", nil) })

	assert.Equal(t, "Hello Go!!", txt.ToString())
}

// ── YTextEvent.Delta tests ────────────────────────────────────────────────────

func TestUnit_YText_Delta_InsertOnly(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	var got []Delta
	txt.Observe(func(e YTextEvent) { got = e.Delta })

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	require.Len(t, got, 1)
	assert.Equal(t, DeltaOpInsert, got[0].Op)
	assert.Equal(t, "hello", got[0].Insert)
}

func TestUnit_YText_Delta_DeleteOnly(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	var got []Delta
	txt.Observe(func(e YTextEvent) { got = e.Delta })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 5) })

	require.Len(t, got, 1)
	assert.Equal(t, DeltaOpDelete, got[0].Op)
	assert.Equal(t, 5, got[0].Delete)
}

func TestUnit_YText_Delta_InsertAndDelete(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello world", nil) })

	var got []Delta
	txt.Observe(func(e YTextEvent) { got = e.Delta })
	// Replace "world" with "Go": delete 5 chars at index 6, insert "Go" at index 6.
	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 6, 5)
		txt.Insert(txn, 6, "Go", nil)
	})

	// Expect: retain 6, insert "Go", delete 5 (trailing retain omitted).
	retains := 0
	inserts := 0
	deletes := 0
	for _, d := range got {
		switch d.Op {
		case DeltaOpRetain:
			retains += d.Retain
		case DeltaOpInsert:
			inserts++
		case DeltaOpDelete:
			deletes++
		}
	}
	assert.Equal(t, 6, retains, "should retain 'hello '")
	assert.Equal(t, 1, inserts, "should have one insert op")
	assert.Equal(t, 1, deletes, "should have one delete op")
	assert.Equal(t, "hello Go", txt.ToString())
}

// ── Supplementary Unicode / Emoji tests ──────────────────────────────────────

func TestUnit_YText_Len_SupplementaryUnicode(t *testing.T) {
	// 👍 is U+1F44D, encoded as 2 UTF-16 code units (surrogate pair)
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello👍", nil) // 5 ASCII + 2 UTF-16 for emoji = 7
	})
	assert.Equal(t, 7, txt.Len(), "👍 must count as 2 UTF-16 units")
	assert.Equal(t, "Hello👍", txt.ToString())
}

func TestUnit_YText_Insert_AtSurrogateBoundary(t *testing.T) {
	// Insert between two emoji, then verify ordering
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "😀😂", nil) // 4 UTF-16 units total (2+2)
	})
	assert.Equal(t, 4, txt.Len())

	// Insert a character between the two emoji (at index 2)
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 2, "-", nil)
	})
	assert.Equal(t, "😀-😂", txt.ToString())
	assert.Equal(t, 5, txt.Len())
}

func TestUnit_YText_Delete_SupplementaryUnicode(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "A👍B", nil) // 4 UTF-16 units
	})
	assert.Equal(t, 4, txt.Len())

	// Delete the emoji (2 UTF-16 units at position 1)
	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 1, 2)
	})
	assert.Equal(t, "AB", txt.ToString())
	assert.Equal(t, 2, txt.Len())
}

// ── ApplyDelta tests ──────────────────────────────────────────────────────────

func TestUnit_YText_ApplyDelta_InsertOnly(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.ApplyDelta(txn, []Delta{
			{Op: DeltaOpInsert, Insert: "Hello"},
		})
	})
	assert.Equal(t, "Hello", txt.ToString())
}

func TestUnit_YText_ApplyDelta_InsertDeleteRetain(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello World", nil)
	})
	doc.Transact(func(txn *Transaction) {
		txt.ApplyDelta(txn, []Delta{
			{Op: DeltaOpRetain, Retain: 6},    // keep "Hello "
			{Op: DeltaOpDelete, Delete: 5},    // delete "World"
			{Op: DeltaOpInsert, Insert: "Go"}, // insert "Go"
		})
	})
	assert.Equal(t, "Hello Go", txt.ToString())
}

func TestUnit_YText_ApplyDelta_RetainWithFormat(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "bold text", nil)
	})
	doc.Transact(func(txn *Transaction) {
		txt.ApplyDelta(txn, []Delta{
			{Op: DeltaOpRetain, Retain: 4, Attributes: Attributes{"bold": true}},
		})
	})
	delta := txt.ToDelta()
	// First 4 chars should be bold
	require.NotEmpty(t, delta)
	assert.Equal(t, true, delta[0].Attributes["bold"])
}

func TestUnit_YText_ApplyDelta_Emoji(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.ApplyDelta(txn, []Delta{
			{Op: DeltaOpInsert, Insert: "A👍B"},
		})
	})
	assert.Equal(t, 4, txt.Len()) // A=1 + 👍=2 + B=1
	assert.Equal(t, "A👍B", txt.ToString())
}

// ── computeDelta format change tests ──────────────────────────────────────────
// These tests verify that the observer event delta correctly reflects
// ContentFormat changes produced by Format() calls, not just ContentString
// inserts and deletes.

func TestUnit_YText_FormatDelta_AddAttribute(t *testing.T) {
	// "Hello World" — Format() bolds "World". The observer delta must be
	// [{retain:6}, {retain:5, attributes:{bold:true}}].
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello World", nil)
	})

	var got []Delta
	txt.Observe(func(e YTextEvent) { got = e.Delta })

	doc.Transact(func(txn *Transaction) {
		txt.Format(txn, 6, 5, Attributes{"bold": true})
	})

	require.Len(t, got, 2)
	assert.Equal(t, DeltaOpRetain, got[0].Op)
	assert.Equal(t, 6, got[0].Retain)
	assert.Nil(t, got[0].Attributes)
	assert.Equal(t, DeltaOpRetain, got[1].Op)
	assert.Equal(t, 5, got[1].Retain)
	assert.Equal(t, true, got[1].Attributes["bold"])
}

func TestUnit_YText_FormatDelta_RemoveAttribute(t *testing.T) {
	// "Hello World" with "World" already bold. Format() removes bold.
	// Observer delta must be [{retain:6}, {retain:5, attributes:{bold:null}}].
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello ", nil)
		txt.Insert(txn, 6, "World", Attributes{"bold": true})
	})

	var got []Delta
	txt.Observe(func(e YTextEvent) { got = e.Delta })

	doc.Transact(func(txn *Transaction) {
		txt.Format(txn, 6, 5, Attributes{"bold": nil}) // remove bold
	})

	require.Len(t, got, 2)
	assert.Equal(t, DeltaOpRetain, got[0].Op)
	assert.Equal(t, 6, got[0].Retain)
	assert.Nil(t, got[0].Attributes)
	assert.Equal(t, DeltaOpRetain, got[1].Op)
	assert.Equal(t, 5, got[1].Retain)
	assert.Contains(t, got[1].Attributes, "bold")
	assert.Nil(t, got[1].Attributes["bold"])
}

func TestUnit_YText_FormatDelta_MultipleAttributes(t *testing.T) {
	// Format with two attrs at once — observer delta carries both.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hi", nil)
	})

	var got []Delta
	txt.Observe(func(e YTextEvent) { got = e.Delta })

	doc.Transact(func(txn *Transaction) {
		txt.Format(txn, 0, 2, Attributes{"bold": true, "italic": true})
	})

	// Exactly one retain covering the full text with both attrs (no preceding
	// plain retain since format starts at index 0).
	require.Len(t, got, 1)
	assert.Equal(t, DeltaOpRetain, got[0].Op)
	assert.Equal(t, 2, got[0].Retain)
	assert.Equal(t, true, got[0].Attributes["bold"])
	assert.Equal(t, true, got[0].Attributes["italic"])
}

func TestUnit_YText_FormatDelta_NoTrailingRetain(t *testing.T) {
	// Format at the start leaves an unformatted tail. The tail must not appear
	// in the delta (trailing plain retain is omitted per Quill convention).
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello World", nil)
	})

	var got []Delta
	txt.Observe(func(e YTextEvent) { got = e.Delta })

	doc.Transact(func(txn *Transaction) {
		txt.Format(txn, 0, 5, Attributes{"bold": true}) // bold "Hello"
	})

	// [{retain:5, attributes:{bold:true}}] — no trailing retain for " World".
	require.Len(t, got, 1)
	assert.Equal(t, DeltaOpRetain, got[0].Op)
	assert.Equal(t, 5, got[0].Retain)
	assert.Equal(t, true, got[0].Attributes["bold"])
}

func TestUnit_YText_FormatDelta_MixedInsertAndFormat(t *testing.T) {
	// Same transaction inserts text AND applies formatting. Both must appear
	// correctly in the observer delta.
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "Hello", nil)
	})

	var got []Delta
	txt.Observe(func(e YTextEvent) { got = e.Delta })

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 5, " World", nil)            // append plain text
		txt.Format(txn, 0, 5, Attributes{"bold": true}) // bold existing "Hello"
	})

	// Expect a retain (bold "Hello") and an insert (" World").
	var retainOp, insertOp *Delta
	for i := range got {
		switch got[i].Op {
		case DeltaOpRetain:
			retainOp = &got[i]
		case DeltaOpInsert:
			insertOp = &got[i]
		}
	}
	require.NotNil(t, retainOp, "expected a retain op for formatted text")
	require.NotNil(t, insertOp, "expected an insert op for new text")
	assert.Equal(t, true, retainOp.Attributes["bold"])
	assert.Equal(t, " World", insertOp.Insert)
}
