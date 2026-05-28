package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression guards for the v1.16.0 perf PR (#86 + #54).

// #86 — firstLiveFromStart must correctly skip leading tombstones and
// remain valid across sequential head-deletes. Pre-fix the cleanup walk
// rescanned all tombstones each time, producing O(N²) behaviour for the
// "1000 head-deletes" workload that BenchmarkYText_Delete exercises.
func TestUnit_FirstLiveFromStart_SkipsLeadingTombstones(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcdef", nil) })

	// After 3 sequential head-deletes the live region is "def" — firstLive
	// must point at the item containing 'd' (or a later live item, depending
	// on how Delete splits).
	doc.Transact(func(txn *Transaction) {
		txt.Delete(txn, 0, 1)
		txt.Delete(txn, 0, 1)
		txt.Delete(txn, 0, 1)
	})

	live := txt.firstLiveFromStart()
	require.NotNil(t, live, "must find a live item after partial head-delete")
	assert.False(t, live.Deleted, "firstLive must not be tombstoned")
	assert.Equal(t, "def", txt.ToString(),
		"content must round-trip unchanged")
}

// #86 — inserting at index 0 must invalidate the firstLive cache so the
// new head is picked up on the next cleanup walk.
func TestUnit_FirstLiveFromStart_ResetsOnHeadInsert(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "world", nil) })

	// Prime the cache.
	first := txt.firstLiveFromStart()
	require.NotNil(t, first)

	// Insert a new head item.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello ", nil) })

	live := txt.firstLiveFromStart()
	require.NotNil(t, live)
	assert.NotEqual(t, first, live,
		"firstLive must update after a head-insert (cache invalidation)")
	// The new live item should be the one containing the head of "hello ",
	// which is now txt.start.
	assert.Equal(t, txt.start, live)
}

// #86 — every item tombstoned still returns nil (no infinite loop, no panic).
func TestUnit_FirstLiveFromStart_AllDeleted(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 3) })

	assert.Nil(t, txt.firstLiveFromStart(),
		"firstLive must be nil when every item is tombstoned")
}

// #54 A — Transact must produce a transaction whose pre-sized fields can
// absorb a typical workload (1-3 types) without rehashing the map.
// (newItems is intentionally left nil-init since pre-sizing it added one
// alloc per txn for workloads that never insert ContentString.)
func TestUnit_Transaction_PresizedFields(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "x", nil)
		// changed must already record txt's abstractType.
		assert.Contains(t, txn.changed, &txt.abstractType,
			"changed map must record modifications during the txn")
		// newItems must include the just-inserted ContentString.
		assert.GreaterOrEqual(t, len(txn.newItems), 1,
			"newItems must collect ContentString inserts")
	})
}

// #86 — plain-text YText (never had Format() called) must not set
// hasFormatting, so YText.Delete skips the cleanup walk entirely.
func TestUnit_YText_HasFormatting_FalseForPlainText(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello world", nil) })

	assert.False(t, txt.hasFormatting,
		"hasFormatting must stay false until a ContentFormat is integrated")
}

// #86 — YText.Format integrates ContentFormat markers, which must flip
// hasFormatting to true so subsequent Delete calls run the cleanup walk.
func TestUnit_YText_HasFormatting_TrueAfterFormat(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		txt.Format(txn, 0, 5, Attributes{"bold": true})
	})

	assert.True(t, txt.hasFormatting,
		"Format() integrates ContentFormat → hasFormatting must become true")
}

// #86 — once hasFormatting is true, it stays true even after all format
// markers are deleted (matches Yjs's _hasFormatting behaviour).
func TestUnit_YText_HasFormatting_StaysTrueAfterFormatDeleted(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		txt.Format(txn, 0, 5, Attributes{"bold": true})
	})
	require.True(t, txt.hasFormatting)

	// Delete the whole formatted span (which also tombstones the markers).
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, txt.Len()) })

	assert.True(t, txt.hasFormatting,
		"hasFormatting must remain true after all formats are deleted, "+
			"matching Yjs's once-true-always-true semantics")
}
