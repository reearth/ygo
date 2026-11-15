package crdt

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// A truncated apply records integrated and parked data after transaction GC;
// replay restores both the visible map value and the pending queue.
func TestUnit_ApplyUpdateV1Landed_EncodedAfterGC(t *testing.T) {
	for _, gc := range []bool{true, false} {
		t.Run(map[bool]string{true: "GC on", false: "GC off"}[gc], func(t *testing.T) { landedEqualsOnUpdate(t, gc) })
	}
}

func landedEqualsOnUpdate(t *testing.T, gc bool) {
	author := newTestDoc(1)
	am := author.GetMap("m")
	author.Transact(func(txn *Transaction) { am.Set(txn, "nested", NewMapPrelim()) })
	editor := New(WithClientID(7), WithGC(false))
	require.NoError(t, ApplyUpdateV1(editor, EncodeStateAsUpdateV1(author, nil), nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested := v.(*YMap)
	ek := editor.GetMap("k")
	before := editor.StateVector()
	editor.Transact(func(txn *Transaction) { ek.Set(txn, "a", "first") })
	editor.Transact(func(txn *Transaction) { ek.Set(txn, "a", "second") })
	editor.Transact(func(txn *Transaction) {
		for i := range 4 {
			nested.Set(txn, "e"+strconv.Itoa(i), i) // parks: the room lacks nested
		}
	})
	overflowing := EncodeStateAsUpdateV1(editor, before)

	room := New(WithClientID(100), WithMaxPendingItems(10), WithGC(gc))
	room.GetMap("k")
	var published [][]byte
	room.OnUpdate(func(u []byte, _ any) { published = append(published, u) })
	// A truncated delete set fails after the valid structs have integrated or parked.
	landed, err := ApplyUpdateV1Landed(room, overflowing[:len(overflowing)-1], nil)
	require.ErrorIs(t, err, ErrInvalidUpdate)
	v, _ = room.GetMap("k").Get("a")
	require.Equal(t, "second", v, "the overwrite integrated")
	require.Len(t, published, 1)
	replay := New(WithClientID(101), WithGC(gc))
	replay.GetMap("k")
	require.NoError(t, ApplyUpdateV1(replay, landed, nil))
	v, _ = replay.GetMap("k").Get("a")
	require.Equal(t, "second", v)
	require.Equal(t, room.PendingStats().Items, replay.PendingStats().Items)
	visibleOnly := New(WithClientID(102), WithGC(gc))
	visibleOnly.GetMap("k")
	require.NoError(t, ApplyUpdateV1(visibleOnly, published[0], nil))
	v, _ = visibleOnly.GetMap("k").Get("a")
	require.Equal(t, "second", v)
	require.Zero(t, visibleOnly.PendingStats().Items, "OnUpdate excludes parked structs")
}

// TestUnit_ApplyUpdateV1Landed_SuccessAllocationOverheadIsBounded bounds the
// successful apply overhead introduced by landed-update bookkeeping.
func TestUnit_ApplyUpdateV1Landed_SuccessAllocationOverheadIsBounded(t *testing.T) {
	author := newTestDoc(1)
	txt := author.GetText("t")
	author.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abc", nil) })
	update := EncodeStateAsUpdateV1(author, nil)
	// The fewest over a few measurements: a GC cycle or another goroutine
	// during one measurement only adds allocations to it.
	fewest := func(f func()) float64 {
		n := testing.AllocsPerRun(50, f)
		for range 4 {
			n = min(n, testing.AllocsPerRun(50, f))
		}
		return n
	}
	apply := fewest(func() {
		require.NoError(t, ApplyUpdateV1(newTestDoc(2), update, nil))
	})
	landed := fewest(func() {
		_, err := ApplyUpdateV1Landed(newTestDoc(2), update, nil)
		require.NoError(t, err)
	})
	// Held/landed bookkeeping adds at most one allocation to a successful apply.
	require.LessOrEqual(t, landed, apply+1)
}
