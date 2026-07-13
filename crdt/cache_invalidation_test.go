package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFirstLiveCache_StaleAfterInsertAfterTombstone is #70 residual class 1.
//
// Inserting after leading tombstones (the anchor the YText tombstone-skip fix
// produces) makes a new live run that is NOT the list head, so item.integrate's
// new-head branch — the only place firstLiveCache is invalidated on insert —
// never fires. deleteRange then walks from a stale firstLiveFromStart and
// deletes the wrong indices. Here the final Delete(2,2) silently no-ops
// (leaving "edfbγ") instead of removing "fb" (→ "edγ").
func TestFirstLiveCache_StaleAfterInsertAfterTombstone(t *testing.T) {
	c2 := New(WithClientID(2))
	t2 := c2.GetText("t")
	c2.Transact(func(txn *Transaction) { t2.Insert(txn, 0, "dd", nil) }) // "dd"
	c2.Transact(func(txn *Transaction) { t2.Delete(txn, 0, 2) })         // "" — two leading tombstones

	// Remote "γ本δ" from another client.
	c4 := New(WithClientID(4))
	t4 := c4.GetText("t")
	c4.Transact(func(txn *Transaction) { t4.Insert(txn, 0, "γ本δ", nil) })
	require.NoError(t, ApplyUpdateV1(c2, EncodeStateAsUpdateV1(c4, nil), nil))
	require.Equal(t, "γ本δ", mustText(t, c2, "t"))

	c2.Transact(func(txn *Transaction) { t2.Delete(txn, 1, 2) })           // "γ"
	c2.Transact(func(txn *Transaction) { t2.Insert(txn, 0, "edfb", nil) }) // "edfbγ"
	c2.Transact(func(txn *Transaction) { t2.Delete(txn, 2, 2) })           // remove "fb" -> "edγ"

	require.Equal(t, "edγ", mustText(t, c2, "t"),
		"Delete after insert-past-tombstone must use the live head, not a stale firstLiveCache")
}

// TestPosCache_StaleAfterRemoteDeleteOnlyApply is #70 residual class 2 — a
// pre-existing bug independent of the YText fix.
//
// transactInternal hardcodes txn.Local=true even for ApplyUpdate, so
// item.delete's posCache-clearing branch (`if !txn.Local`) is dead during
// remote applies, and applyToPartial never invalidates posCache. A remote
// update that only tombstones an item leaves posCache stale, so the next local
// positioned insert resolves the wrong neighbour.
func TestPosCache_StaleAfterRemoteDeleteOnlyApply(t *testing.T) {
	docA := New(WithClientID(1))
	arrA := docA.GetArray("a")
	docA.Transact(func(txn *Transaction) {
		for _, v := range []any{"A", "B", "C", "D", "E"} {
			arrA.Push(txn, []any{v})
		}
	})

	docB := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, nil), nil))
	arrB := docB.GetArray("a")

	// Populate docB's posCache with a positioned insert near the end, so live
	// items AFTER the soon-deleted "B" (namely C@3, D@4) get cached. -> [A B C D X E]
	docB.Transact(func(txn *Transaction) { arrB.Insert(txn, 4, []any{"X"}) })

	// docA deletes "B"; ship ONLY that delete to docB. The cached C/D entries
	// stay LIVE but their cumulative index is now off by one.
	var delUpdate []byte
	docA.OnUpdate(func(u []byte, _ any) { delUpdate = u })
	docA.Transact(func(txn *Transaction) { arrA.Delete(txn, 1, 1) })
	require.NoError(t, ApplyUpdateV1(docB, delUpdate, nil)) // docB live: [A C D X E]

	// Positioned insert; must land at live index 3 (before X) -> [A C D Y X E].
	docB.Transact(func(txn *Transaction) { arrB.Insert(txn, 3, []any{"Y"}) })

	got, err := arrB.ToJSON()
	require.NoError(t, err)
	require.JSONEq(t, `["A","C","D","Y","X","E"]`, string(got),
		"positioned insert after a remote delete-only apply must not use a stale posCache")
}

func mustText(t *testing.T, d *Doc, name string) string {
	b, err := d.GetText(name).ToJSON()
	require.NoError(t, err)
	// ToJSON returns a JSON-quoted string; strip the quotes.
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
