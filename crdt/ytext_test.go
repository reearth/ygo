package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
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

// ── YText integration ─────────────────────────────────────────────────────────

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
