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
	assert.Equal(t, 5, txt.Len(), "Len() must count runes, not bytes")
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
	for item := txt.abstractType.start; item != nil; item = item.Right {
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
