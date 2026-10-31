package crdt

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// A failed apply's landed bytes are encoded at commit, after the GC and the
// end-of-transaction merges, as OnUpdate's are, so the two are the same bytes:
// with GC on, the map entry the apply both integrated and overwrote goes out
// as a tombstone in both.
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

	room := New(WithClientID(100), WithMaxPendingItems(2), WithGC(gc))
	room.GetMap("k")
	var published [][]byte
	room.OnUpdate(func(u []byte, _ any) { published = append(published, u) })
	landed, err := ApplyUpdateV1Landed(room, overflowing, nil)
	require.ErrorIs(t, err, ErrInvalidUpdate)
	v, _ = room.GetMap("k").Get("a")
	require.Equal(t, "second", v, "the overwrite integrated")
	require.Len(t, published, 1)
	require.Equal(t, published[0], landed)
}
