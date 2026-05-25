package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for #74 D3 — YTextEvent.computeDelta must emit Delta entries for
// ContentEmbed (and ContentType) items, not just ContentString. Pre-fix,
// the switch only handled ContentString and ContentFormat, so observers
// missed embed inserts and deletes entirely.

// D3 — Embed insert produces a Delta entry in the observer event with the
// embed value as Insert (not a string).
func TestUnit_YTextEvent_DeliversEmbedInsert(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")

	var observed []Delta
	txt.Observe(func(e YTextEvent) { observed = e.Delta })

	embed := map[string]any{"image": "https://example.com/x.png"}
	doc.Transact(func(txn *Transaction) {
		txt.InsertEmbed(txn, 0, embed, nil)
	})

	require.NotEmpty(t, observed,
		"observer must receive at least one Delta entry for the embed insert")
	// Find the entry whose Insert is the embed value (not a string).
	var found bool
	for _, d := range observed {
		if d.Op != DeltaOpInsert {
			continue
		}
		if got, ok := d.Insert.(map[string]any); ok {
			if got["image"] == "https://example.com/x.png" {
				found = true
				break
			}
		}
	}
	assert.True(t, found,
		"observer Delta must carry the embed value as Insert (#74 D3)")
}

// D3 — Embed delete produces a Delta entry of length 1 (embed counts as one
// UTF-16 unit, matching Yjs convention).
func TestUnit_YTextEvent_DeliversEmbedDelete(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	embed := map[string]any{"video": "y.mp4"}
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "before", nil)
		txt.InsertEmbed(txn, 6, embed, nil)
	})

	var observed []Delta
	txt.Observe(func(e YTextEvent) { observed = e.Delta })

	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 6, 1) // delete the embed
	})

	require.NotEmpty(t, observed)
	// Expect a Retain(6) followed by Delete(1).
	var retainSeen, deleteSeen bool
	for _, d := range observed {
		if d.Op == DeltaOpRetain && d.Retain == 6 {
			retainSeen = true
		}
		if d.Op == DeltaOpDelete && d.Delete == 1 {
			deleteSeen = true
		}
	}
	assert.True(t, retainSeen, "must retain past the leading 'before' text")
	assert.True(t, deleteSeen, "must report a 1-unit delete for the embed (#74 D3)")
}

// D3 — Retain past an embed during a subsequent edit advances by 1 (not 0).
func TestUnit_YTextEvent_RetainAcrossEmbed(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	embed := map[string]any{"x": "y"}
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "ab", nil)
		txt.InsertEmbed(txn, 2, embed, nil)
		txt.Insert(txn, 3, "cd", nil)
	})

	var observed []Delta
	txt.Observe(func(e YTextEvent) { observed = e.Delta })

	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 5, "Z", nil) // insert at end (position 5: ab + embed + cd)
	})

	// Expected: Retain(5) + Insert("Z"). The embed contributes 1 to the retain.
	require.NotEmpty(t, observed)
	var totalRetain int
	for _, d := range observed {
		if d.Op == DeltaOpRetain {
			totalRetain += d.Retain
		}
	}
	assert.Equal(t, 5, totalRetain,
		"retain past 'ab' + embed + 'cd' must total 5 (embed counts as 1)")
}
