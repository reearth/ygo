//go:build go1.24

package crdt

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"
	"weak"

	"github.com/stretchr/testify/require"
)

// The test in this file needs the weak package, which Go 1.24 added; the
// module still builds with Go 1.23, which leaves it out.

// A parked string the merge cuts at its front holds only the part it keeps,
// which is what PendingStats.ContentBytes counts. Here a 1 MiB string of
// client 9 parks, and then a deleted struct one clock before it parks and
// covers all of it but its last 16 bytes. The cut used to keep a substring of
// the 1 MiB, and with it the whole string.
func TestUnit_PendingMerge_AParkedStringCutAtItsFrontHoldsOnlyItsTail(t *testing.T) {
	const size, tail = 1 << 20, 16
	str := &Item{ID: ID{Client: 9, Clock: 2}, Origin: &ID{Client: 9, Clock: 1},
		Content: NewContentString(strings.Repeat("x", size-tail) + strings.Repeat("y", tail))}
	cover := &Item{ID: ID{Client: 9, Clock: 1}, Origin: &ID{Client: 9, Clock: 0},
		Content: NewContentDeleted(size - tail + 1)}

	doc := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(doc, structsUpdate(str), nil))
	original := weak.Make(unsafe.StringData(doc.store.pending.heldItems()[0].Content.(*ContentString).Str))
	require.NoError(t, ApplyUpdateV1(doc, structsUpdate(cover), nil))

	held := doc.store.pending.heldItems()
	require.Len(t, held, 2, "the cover and the string's tail are parked")
	var kept *ContentString
	for _, it := range held {
		if s, ok := it.Content.(*ContentString); ok {
			kept = s
		}
	}
	require.NotNil(t, kept, "the string's tail is parked")
	require.Equal(t, strings.Repeat("y", tail), kept.Str)
	require.Equal(t, tail, doc.PendingStats().ContentBytes)
	runtime.GC()
	require.Nil(t, original.Value(), "the 1 MiB string the tail was cut from is still reachable")
	runtime.KeepAlive(doc)
}
