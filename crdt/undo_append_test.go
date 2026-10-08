package crdt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Merging a restored run's content is linear: a run of n strings allocates
// a bounded number of times, not once per piece.
func TestUnit_UndoManager_AppendContents_Linear(t *testing.T) {
	rest := make([]*Item, 2000)
	for i := range rest {
		rest[i] = &Item{Content: NewContentString("é")}
	}
	var got Content
	allocs := testing.AllocsPerRun(5, func() { got = appendContents(NewContentString("ab"), rest) })
	require.Less(t, allocs, 40.0)
	want := NewContentString("ab" + strings.Repeat("é", len(rest)))
	require.Equal(t, want, got)

	anys := appendContents(&ContentAny{Vals: []any{1}}, []*Item{{Content: &ContentAny{Vals: []any{2, 3}}}})
	require.Equal(t, &ContentAny{Vals: []any{1, 2, 3}}, anys)
	js := appendContents(&ContentJSON{Vals: []any{"a"}}, []*Item{{Content: &ContentJSON{Vals: []any{"b"}}}})
	require.Equal(t, &ContentJSON{Vals: []any{"a", "b"}}, js)
}

// Undoing the delete of a long typed run restores it as one string.
func TestUnit_UndoManager_UndoDeleteOfTypedRun_RestoresText(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []SharedType{txt})
	for i := range 300 {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, i, string(rune('a'+i%26)), nil) })
	}
	want := txt.ToString()
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 300) })
	require.True(t, um.Undo())
	require.Equal(t, want, txt.ToString())
}
