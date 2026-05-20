package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for issue #72 — delete-path correctness gaps surfaced by the
// cross-reference audit against Yjs JS and yrs.

// countTombstones returns how many items in the store carry Deleted=true.
// Used to verify that Item.delete propagates into nested ContentType children
// per #72 vector B1.
func countTombstones(doc *Doc) int {
	count := 0
	for _, items := range doc.store.clients {
		for _, item := range items {
			if item.Deleted {
				count++
			}
		}
	}
	return count
}

// B1 (HIGH) — Item.delete must recurse into ContentType children. Pre-fix,
// deleting an outer container only tombstoned the container item; the
// nested map's entries stayed live in the store, the delete-set on the
// wire omitted them, and peers that held the same nested type would see
// inconsistent state.
func TestUnit_Item_Delete_CascadesIntoContentTypeChildren(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("outer")

	// Build a nested YMap and insert it into the outer YArray.
	nested := &YMap{}
	nested.doc = doc
	nested.itemMap = make(map[string]*Item)
	nested.owner = nested

	doc.Transact(func(txn *Transaction) {
		// Wrap the nested type in a ContentType item parented to the array.
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentType(&nested.abstractType),
		}
		item.integrate(txn, 0)

		// Now populate the nested map with three entries.
		nested.Set(txn, "k1", "v1")
		nested.Set(txn, "k2", "v2")
		nested.Set(txn, "k3", "v3")
	})

	require.Equal(t, 0, countTombstones(doc), "no tombstones before delete")

	// Delete the outer entry. Pre-fix, only the outer container item gets
	// tombstoned (1 tombstone). Post-fix, the three nested map entries
	// also cascade (4 tombstones total).
	doc.Transact(func(txn *Transaction) {
		arr.Delete(txn, 0, 1)
	})

	assert.GreaterOrEqual(t, countTombstones(doc), 4,
		"Item.delete must cascade into ContentType children: expected ≥4 tombstones "+
			"(1 outer + 3 nested entries), pre-fix only the outer is marked")
}

// B1 cont. — the cascade must be reachable from the encoded delete-set so
// peers that hold the same nested type also tombstone the inner items.
// Verify by syncing two docs: doc1 deletes the nested-bearing outer; doc2
// must see the nested items as tombstoned after applying the update.
func TestUnit_Item_Delete_Cascade_PropagatesToOtherPeers(t *testing.T) {
	doc1 := newTestDoc(1)
	doc2 := New(WithClientID(2))

	arr1 := doc1.GetArray("outer")
	nested1 := &YMap{}
	nested1.doc = doc1
	nested1.itemMap = make(map[string]*Item)
	nested1.owner = nested1

	doc1.Transact(func(txn *Transaction) {
		at := arr1.baseType()
		item := &Item{
			ID:      ID{Client: doc1.clientID, Clock: doc1.store.NextClock(doc1.clientID)},
			Parent:  at,
			Content: NewContentType(&nested1.abstractType),
		}
		item.integrate(txn, 0)
		nested1.Set(txn, "k1", "v1")
		nested1.Set(txn, "k2", "v2")
	})

	// Sync to doc2.
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, nil), nil))
	require.Equal(t, 0, countTombstones(doc2), "doc2 starts clean")

	// doc1 deletes the outer container.
	doc1.Transact(func(txn *Transaction) { arr1.Delete(txn, 0, 1) })

	// Apply the update to doc2.
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, doc2.StateVector()), nil))

	assert.GreaterOrEqual(t, countTombstones(doc2), 3,
		"after applying remote cascade delete, doc2 must tombstone the outer + nested entries "+
			"(≥3 = 1 outer + 2 nested); pre-fix only the outer arrives in the delete-set")
}

// B2 (HIGH) — DeleteSet.applyToPartial must split items at range boundaries
// before tombstoning. Pre-fix, a partial-range delete on a locally-squashed
// run wiped the entire item, including content outside the declared range.
func TestUnit_DeleteSet_PartialOverlap_SplitsItemBeforeDelete(t *testing.T) {
	// docA inserts "hello" as one squashed run.
	docA := newTestDoc(1)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, 0, "hello", nil) })

	// docB syncs and deletes only "hel" (positions 0..2) locally.
	docB := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, nil), nil))
	txtB := docB.GetText("t")
	docB.Transact(func(txn *Transaction) { txtB.Delete(txn, 0, 3) })
	require.Equal(t, "lo", txtB.ToString(), "local: only 'hel' is deleted, 'lo' remains")

	// Apply docB's update to docA. The delete-set entry is
	// {client:2's view of client 1's clock 0..2}. docA still has the original
	// "hello" as one item; applyToPartial must split it at clock 3 before
	// deleting the left half.
	require.NoError(t, ApplyUpdateV1(docA, EncodeStateAsUpdateV1(docB, docA.StateVector()), nil))

	// Pre-fix: docA shows "" because the whole "hello" item was tombstoned.
	// Post-fix: docA shows "lo" because the item was split before delete.
	assert.Equal(t, "lo", txtA.ToString(),
		"partial-range delete must only tombstone the overlap, not the whole item")
}

// B2 cont. — the range is entirely inside a squashed run. Item must be
// split TWICE (at both boundaries) so the middle slice is tombstoned and
// both ends remain live.
func TestUnit_DeleteSet_RangeStrictlyInsideItem_SplitsTwice(t *testing.T) {
	// docA writes "abcdef" as one squashed run.
	docA := newTestDoc(1)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, 0, "abcdef", nil) })

	docB := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, nil), nil))
	txtB := docB.GetText("t")

	// docB deletes the middle three ("cde", positions 2..4 inclusive).
	docB.Transact(func(txn *Transaction) { txtB.Delete(txn, 2, 3) })
	require.Equal(t, "abf", txtB.ToString(), "local: middle 'cde' removed")

	require.NoError(t, ApplyUpdateV1(docA, EncodeStateAsUpdateV1(docB, docA.StateVector()), nil))
	assert.Equal(t, "abf", txtA.ToString(),
		"range strictly inside an item must split both boundaries, keeping prefix and suffix live")
}

// B2 cont. — when range exactly matches an item span, no split is needed
// and the whole item gets tombstoned (the current behavior is correct in
// this case; pin it so the fix doesn't accidentally introduce spurious
// splits that fragment the linked list).
func TestUnit_DeleteSet_ExactMatch_NoSpuriousSplit(t *testing.T) {
	docA := newTestDoc(1)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, 0, "hi", nil) })

	docB := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, nil), nil))
	txtB := docB.GetText("t")
	itemsBefore := len(docA.store.clients[1])

	docB.Transact(func(txn *Transaction) { txtB.Delete(txn, 0, 2) })
	require.NoError(t, ApplyUpdateV1(docA, EncodeStateAsUpdateV1(docB, docA.StateVector()), nil))

	assert.Equal(t, "", txtA.ToString(), "exact-match delete tombstones the whole item")
	itemsAfter := len(docA.store.clients[1])
	assert.Equal(t, itemsBefore, itemsAfter,
		"no spurious splits when the range exactly matches an item")
}
