package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runFmt builds a YText via build and returns its delta. Expected deltas in
// these tests were captured from the Yjs reference (yjs@13.6.30); the cross-impl
// harness lives in crdt/ytext_format_js_compat_test.go.
func runFmt(t *testing.T, build func(txt *YText, txn *Transaction)) []Delta {
	t.Helper()
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { build(txt, txn) })
	return txt.ToDelta()
}

// F-7 (#123): re-applying a format to a sub-range of an already-formatted run
// must NOT strip formatting from the remainder of the run.

// S3: "aaabbb" bold[0,6) then bold[0,3) → still all bold. (Yjs)
func TestUnit_YText_Format_RebatchSubrange_KeepsRemainder(t *testing.T) {
	d := runFmt(t, func(txt *YText, txn *Transaction) {
		txt.Insert(txn, 0, "aaabbb", nil)
		txt.Format(txn, 0, 6, Attributes{"bold": true})
		txt.Format(txn, 0, 3, Attributes{"bold": true})
	})
	require.Len(t, d, 1)
	assert.Equal(t, "aaabbb", d[0].Insert)
	assert.Equal(t, true, d[0].Attributes["bold"])
}

// S4: "abc DEF ghi", bold[4,7) (DEF), then bold[0,4) → [0,7) bold, " ghi" plain. (Yjs)
func TestUnit_YText_Format_OverlapAtBoundary_KeepsExisting(t *testing.T) {
	d := runFmt(t, func(txt *YText, txn *Transaction) {
		txt.Insert(txn, 0, "abc DEF ghi", nil)
		txt.Format(txn, 4, 3, Attributes{"bold": true})
		txt.Format(txn, 0, 4, Attributes{"bold": true})
	})
	require.Len(t, d, 2)
	assert.Equal(t, "abc DEF", d[0].Insert)
	assert.Equal(t, true, d[0].Attributes["bold"])
	assert.Equal(t, " ghi", d[1].Insert)
	assert.NotEqual(t, true, d[1].Attributes["bold"])
}

// S5: "aaabbb" bold[0,6), then unbold[2,4) → bold,plain,bold (bold restored after). (Yjs)
func TestUnit_YText_Format_UnsetMiddle_RestoresAfterRange(t *testing.T) {
	d := runFmt(t, func(txt *YText, txn *Transaction) {
		txt.Insert(txn, 0, "aaabbb", nil)
		txt.Format(txn, 0, 6, Attributes{"bold": true})
		txt.Format(txn, 2, 2, Attributes{"bold": nil})
	})
	require.Len(t, d, 3)
	assert.Equal(t, "aa", d[0].Insert)
	assert.Equal(t, true, d[0].Attributes["bold"])
	assert.Equal(t, "ab", d[1].Insert)
	assert.NotEqual(t, true, d[1].Attributes["bold"])
	assert.Equal(t, "bb", d[2].Insert)
	assert.Equal(t, true, d[2].Attributes["bold"])
}

// Re-applying a value already in effect over a sub-range is a no-op on the delta.
func TestUnit_YText_Format_AlreadyInEffect_NoChange(t *testing.T) {
	d := runFmt(t, func(txt *YText, txn *Transaction) {
		txt.Insert(txn, 0, "hello world", nil)
		txt.Format(txn, 0, 11, Attributes{"bold": true})
		txt.Format(txn, 3, 5, Attributes{"bold": true})
	})
	require.Len(t, d, 1)
	assert.Equal(t, "hello world", d[0].Insert)
	assert.Equal(t, true, d[0].Attributes["bold"])
}

// Unset a leading sub-range, leaving the tail formatted.
func TestUnit_YText_Format_UnsetPrefix_KeepsTail(t *testing.T) {
	d := runFmt(t, func(txt *YText, txn *Transaction) {
		txt.Insert(txn, 0, "aaabbb", nil)
		txt.Format(txn, 0, 6, Attributes{"bold": true})
		txt.Format(txn, 0, 3, Attributes{"bold": nil})
	})
	require.Len(t, d, 2)
	assert.Equal(t, "aaa", d[0].Insert)
	assert.NotEqual(t, true, d[0].Attributes["bold"])
	assert.Equal(t, "bbb", d[1].Insert)
	assert.Equal(t, true, d[1].Attributes["bold"])
}

// F-7 on a doc LOADED via ApplyUpdate (un-split run from the wire). Mirrors
// yjs#606: the over-delete bites hardest on decoded un-split runs. The boundary
// split inside Format must still preserve the surrounding run. (Yjs 13.6.30)
func TestUnit_YText_Format_LoadedDoc_RebatchSubrange(t *testing.T) {
	init := newTestDoc(1)
	it := init.GetText("t")
	init.Transact(func(txn *Transaction) {
		it.Insert(txn, 0, "aaabbb", nil)
		it.Format(txn, 0, 6, Attributes{"bold": true})
	})
	upd := EncodeStateAsUpdateV1(init, nil)

	d := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(d, upd, nil))
	txt := d.GetText("t")
	d.Transact(func(txn *Transaction) { txt.Format(txn, 0, 3, Attributes{"bold": true}) })

	dd := txt.ToDelta()
	require.Len(t, dd, 1)
	assert.Equal(t, "aaabbb", dd[0].Insert)
	assert.Equal(t, true, dd[0].Attributes["bold"])
}
