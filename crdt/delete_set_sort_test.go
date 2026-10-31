package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_DeleteSet_SortAndCompactAllocatesNothing(t *testing.T) {
	unsorted := []DeleteRange{{Clock: 9, Len: 1}, {Clock: 0, Len: 2}, {Clock: 4, Len: 3}, {Clock: 2, Len: 1}, {Clock: 5, Len: 1}}
	ranges := make([]DeleteRange, len(unsorted))
	ds := newDeleteSet()
	allocs := testing.AllocsPerRun(10, func() {
		copy(ranges, unsorted)
		ds.clients[1] = ranges
		ds.sortAndCompact(1)
	})
	require.Zero(t, allocs)
	require.Equal(t, []DeleteRange{{Clock: 0, Len: 3}, {Clock: 4, Len: 3}, {Clock: 9, Len: 1}}, ds.clients[1])
}
