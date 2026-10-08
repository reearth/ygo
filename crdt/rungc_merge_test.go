package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A key's tombstoned value chain merged by RunGC must not leave the map's
// per-key entry pointing at the absorbed item: a later Set on that key would
// link after an off-list node and lose a concurrent same-key conflict.
func TestInteg_RunGC_MapKeyEntryAfterMerge(t *testing.T) {
	a := New(WithClientID(20), WithGC(true))
	m := a.GetMap("m")
	a.Transact(func(txn *Transaction) { m.Set(txn, "k", 1) })
	a.Transact(func(txn *Transaction) { m.Set(txn, "k", 2) })
	a.Transact(func(txn *Transaction) { m.Delete(txn, "k") })

	b := New(WithClientID(10))
	require.NoError(t, ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, nil), nil))

	RunGC(a)
	a.Transact(func(txn *Transaction) { m.Set(txn, "k", "a") })
	bm := b.GetMap("m")
	b.Transact(func(txn *Transaction) { bm.Set(txn, "k", "b") })

	ua := EncodeStateAsUpdateV1(a, nil)
	ub := EncodeStateAsUpdateV1(b, nil)
	require.NoError(t, ApplyUpdateV1(a, ub, nil))
	require.NoError(t, ApplyUpdateV1(b, ua, nil))

	fresh := New(WithClientID(30))
	require.NoError(t, ApplyUpdateV1(fresh, EncodeStateAsUpdateV1(a, nil), nil))

	want, _ := bm.Get("k")
	got, _ := m.Get("k")
	reloaded, _ := fresh.GetMap("m").Get("k")
	require.Equal(t, want, got)
	require.Equal(t, want, reloaded)
}

// Tombstones of different keys can sit adjacent with contiguous clocks: a
// first-time Set has no origin and its conflict scan can stop after another
// key's item. Merging them re-keys the right half, and a later Set whose
// origin is that half would decode under the wrong key.
func TestInteg_RunGC_NoMergeAcrossMapKeys(t *testing.T) {
	b := New(WithClientID(10))
	bm := b.GetMap("m")
	b.Transact(func(txn *Transaction) { bm.Set(txn, "k0", "b") })

	a := New(WithClientID(20), WithGC(true))
	m := a.GetMap("m")
	require.NoError(t, ApplyUpdateV1(a, EncodeStateAsUpdateV1(b, nil), nil))
	a.Transact(func(txn *Transaction) { m.Set(txn, "k0", 0) }) // (20,0), after b's k0
	a.Transact(func(txn *Transaction) { m.Set(txn, "k2", 1) }) // (20,1), no origin, lands after (20,0)
	a.Transact(func(txn *Transaction) { m.Set(txn, "k2", 2) }) // (20,2), origin (20,1)
	a.Transact(func(txn *Transaction) { m.Delete(txn, "k0") })

	RunGC(a)

	fresh := New(WithClientID(30))
	require.NoError(t, ApplyUpdateV1(fresh, EncodeStateAsUpdateV1(a, nil), nil))
	require.Equal(t, m.Entries(), fresh.GetMap("m").Entries())
}
