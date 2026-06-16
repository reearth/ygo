package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F-6 (#125): MergeUpdatesV1 must preserve structs that cannot integrate on
// their own (dependency in a prior update) instead of dropping them.
func TestUnit_MergeUpdatesV1_PreservesNonIntegrableStruct(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tx *Transaction) { txt.Insert(tx, 0, "A", nil) })
	svAfterA := d.StateVector()
	updA := EncodeStateAsUpdateV1(d, nil) // full state = just "A"
	d.Transact(func(tx *Transaction) { txt.Insert(tx, 1, "B", nil) })

	diff := EncodeStateAsUpdateV1(d, svAfterA) // carries only "B" (origin = "A")

	merged, err := MergeUpdatesV1(diff)
	require.NoError(t, err)

	base := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(base, updA, nil))   // base has "A"
	require.NoError(t, ApplyUpdateV1(base, merged, nil)) // apply merged diff
	require.Equal(t, "AB", base.GetText("t").ToString(), "merged diff must still carry B")
}

// TestUnit_MergeUpdatesV1_MapKeyChain folds incremental YMap.Set updates and
// asserts the merged result still carries the key. Logs decoded structs.
func TestUnit_MergeUpdatesV1_MapKeyChain(t *testing.T) {
	doc := New(WithClientID(1))
	m := doc.GetMap("meta")
	var updates [][]byte
	doc.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	for i := 0; i < 3; i++ {
		doc.Transact(func(txn *Transaction) { m.Set(txn, "title", i) })
	}

	// Fold incrementally (simulates persistence). The origin chain linking each
	// keyed item to the previous must survive every fold, or the key is lost.
	merged := updates[0]
	var err error
	for i := 1; i < len(updates); i++ {
		merged, err = MergeUpdatesV1(merged, updates[i])
		require.NoError(t, err)
	}

	d2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(d2, merged, nil))
	v, ok := d2.GetMap("meta").Get("title")
	require.True(t, ok, "key 'title' lost after struct-level merge")
	assert.Equal(t, int64(2), v, "newest value wins after merge")
}

// DiffUpdateV1 returns what a peer is missing and round-trips through apply.
func TestUnit_DiffUpdateV1_RoundTrip(t *testing.T) {
	a := New(WithClientID(1))
	ta := a.GetText("t")
	a.Transact(func(tx *Transaction) { ta.Insert(tx, 0, "hello", nil) })

	b := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, nil), nil))
	a.Transact(func(tx *Transaction) { ta.Insert(tx, 5, " world", nil) })

	diff, err := DiffUpdateV1(EncodeStateAsUpdateV1(a, nil), b.StateVector())
	require.NoError(t, err)
	require.NoError(t, ApplyUpdateV1(b, diff, nil))
	require.Equal(t, "hello world", b.GetText("t").ToString())
}

// EncodeStateVectorFromUpdate reports the next clock per client carried by an
// update, matching EncodeStateVectorV1 of a doc that applied it.
func TestUnit_EncodeStateVectorFromUpdate_MatchesDoc(t *testing.T) {
	a := New(WithClientID(7))
	ta := a.GetText("t")
	a.Transact(func(tx *Transaction) { ta.Insert(tx, 0, "hello", nil) })
	upd := EncodeStateAsUpdateV1(a, nil)

	fromUpdate, err := EncodeStateVectorFromUpdate(upd)
	require.NoError(t, err)
	assert.Equal(t, EncodeStateVectorV1(a), fromUpdate)
}
