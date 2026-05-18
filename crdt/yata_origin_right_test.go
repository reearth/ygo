package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for issues #65 and #68 — YATA OriginRight handling.
//
// Root cause: Item.integrate's conflict-scan loop in crdt/item.go terminates
// on `o != item.Right`, but item.Right is never resolved from item.OriginRight
// before the loop runs. When an incoming item declares a right boundary via
// OriginRight, the scan has no upper bound and walks past concurrent items
// that should not have been crossed, placing the item past its correct
// position.
//
// Yjs JS handles this by calling `getItemCleanStart(transaction, this.rightOrigin)`
// at the top of integrate(). yrs does the same.
//
// Each test below sets up a concurrent scenario where convergence depends on
// the right-boundary being respected. All assert that doc A (which receives
// the remote update) and doc B (which made the local edit) reach the same
// final state.

// Scenario 1 (#65 itself): A concurrent insert between two existing items
// must respect its OriginRight so that the conflict scan stops at the right
// neighbour rather than walking past it.
func TestYATA_OriginRight_BasicRightBoundary(t *testing.T) {
	docA := newTestDoc(1)
	txtA := docA.GetText("content")
	docA.Transact(func(txn *Transaction) { txtA.Insert(txn, 0, "ab", nil) })

	// Sync A → B.
	docB := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(docB, EncodeStateAsUpdateV1(docA, nil), nil))

	// B inserts "X" between "a" and "b". The X item has Origin=a, OriginRight=b.
	txtB := docB.GetText("content")
	docB.Transact(func(txn *Transaction) { txtB.Insert(txn, 1, "X", nil) })
	require.Equal(t, "aXb", txtB.ToString(), "local insert should yield aXb on B")

	// Sync B → A. A must place X between a and b, not after b.
	require.NoError(t, ApplyUpdateV1(docA, EncodeStateAsUpdateV1(docB, docA.StateVector()), nil))
	assert.Equal(t, "aXb", txtA.ToString(), "remote insert must respect OriginRight")
}

// Scenario 3 (audit): Three peers each insert at the same position with
// different right-origins. Each insertion's OriginRight must be respected to
// prevent insertions from "leapfrogging" each other.
func TestYATA_OriginRight_ConflictingRightOrigins(t *testing.T) {
	// Seed: "ab" on doc1.
	doc1 := newTestDoc(1)
	t1 := doc1.GetText("content")
	doc1.Transact(func(txn *Transaction) { t1.Insert(txn, 0, "ab", nil) })

	// doc2 syncs from doc1, then inserts "Y" between a and b.
	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, nil), nil))
	t2 := doc2.GetText("content")
	doc2.Transact(func(txn *Transaction) { t2.Insert(txn, 1, "Y", nil) })

	// doc3 syncs from doc1, then inserts "Z" between a and b — concurrent with doc2's Y.
	doc3 := New(WithClientID(3))
	require.NoError(t, ApplyUpdateV1(doc3, EncodeStateAsUpdateV1(doc1, nil), nil))
	t3 := doc3.GetText("content")
	doc3.Transact(func(txn *Transaction) { t3.Insert(txn, 1, "Z", nil) })

	// Cross-apply: doc1 gets both, in order Y then Z.
	require.NoError(t, ApplyUpdateV1(doc1, EncodeStateAsUpdateV1(doc2, doc1.StateVector()), nil))
	require.NoError(t, ApplyUpdateV1(doc1, EncodeStateAsUpdateV1(doc3, doc1.StateVector()), nil))

	// Apply in reverse order to doc2 and doc3 to test commutativity.
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc3, doc2.StateVector()), nil))
	require.NoError(t, ApplyUpdateV1(doc3, EncodeStateAsUpdateV1(doc2, doc3.StateVector()), nil))

	// All three docs must converge.
	got1 := t1.ToString()
	got2 := t2.ToString()
	got3 := t3.ToString()
	assert.Equal(t, got1, got2, "doc1 and doc2 must converge")
	assert.Equal(t, got1, got3, "doc1 and doc3 must converge")
	// YATA tie-break: lower ClientID wins left position. Both Y and Z share
	// Origin=a, OriginRight=b. Y has ClientID=2, Z has ClientID=3 → Y is placed
	// left of Z. Expected: "aYZb".
	assert.Equal(t, "aYZb", got1, "YATA tie-break: lower ClientID wins left")
}

// Scenario 4 (audit): The right-origin reference points to an item that has
// been split by an intervening concurrent operation. The split target must be
// resolved correctly so the right boundary is honored.
func TestYATA_OriginRight_AcrossSplitBoundary(t *testing.T) {
	// Seed: "abcd" as a single multi-char item on doc1.
	doc1 := newTestDoc(1)
	t1 := doc1.GetText("content")
	doc1.Transact(func(txn *Transaction) { t1.Insert(txn, 0, "abcd", nil) })

	// doc2 syncs, then inserts "X" between "a" and "b" — splits "abcd" into
	// "a" (clock 0), "bcd" (clock 1..3) on doc2.
	doc2 := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, nil), nil))
	t2 := doc2.GetText("content")
	doc2.Transact(func(txn *Transaction) { t2.Insert(txn, 1, "X", nil) })
	require.Equal(t, "aXbcd", t2.ToString())

	// doc3 syncs from doc1 (still has un-split "abcd"), then inserts "Y"
	// between "c" and "d" — its OriginRight points to clock 3 of client 1,
	// which is in the middle of doc1's "abcd" item.
	doc3 := New(WithClientID(3))
	require.NoError(t, ApplyUpdateV1(doc3, EncodeStateAsUpdateV1(doc1, nil), nil))
	t3 := doc3.GetText("content")
	doc3.Transact(func(txn *Transaction) { t3.Insert(txn, 3, "Y", nil) })
	require.Equal(t, "abcYd", t3.ToString())

	// Apply doc2's update to doc1 then doc3, and doc3's to doc1 then doc2.
	require.NoError(t, ApplyUpdateV1(doc1, EncodeStateAsUpdateV1(doc2, doc1.StateVector()), nil))
	require.NoError(t, ApplyUpdateV1(doc1, EncodeStateAsUpdateV1(doc3, doc1.StateVector()), nil))
	require.NoError(t, ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc3, doc2.StateVector()), nil))
	require.NoError(t, ApplyUpdateV1(doc3, EncodeStateAsUpdateV1(doc2, doc3.StateVector()), nil))

	got1, got2, got3 := t1.ToString(), t2.ToString(), t3.ToString()
	assert.Equal(t, got1, got2, "convergence required")
	assert.Equal(t, got1, got3, "convergence required")
	// Expected: X between a and b, Y between c and d.
	assert.Equal(t, "aXbcYd", got1, "right-boundary across split must be respected")
}

// Scenario 5 (audit): Three concurrent insertions, each at a distinct position
// with a distinct OriginRight. Exercises termination of the conflict-scan
// loop in a more crowded conflict zone.
func TestYATA_OriginRight_ThreeConcurrentInsertsDistinctOriginRights(t *testing.T) {
	// Seed: "abcd" on doc1, with each character as a separate item.
	doc1 := newTestDoc(1)
	t1 := doc1.GetText("content")
	doc1.Transact(func(txn *Transaction) {
		t1.Insert(txn, 0, "a", nil)
		t1.Insert(txn, 1, "b", nil)
		t1.Insert(txn, 2, "c", nil)
		t1.Insert(txn, 3, "d", nil)
	})

	// Three docs each sync and concurrently insert at different positions.
	doc2 := New(WithClientID(2))
	doc3 := New(WithClientID(3))
	doc4 := New(WithClientID(4))
	state1 := EncodeStateAsUpdateV1(doc1, nil)
	require.NoError(t, ApplyUpdateV1(doc2, state1, nil))
	require.NoError(t, ApplyUpdateV1(doc3, state1, nil))
	require.NoError(t, ApplyUpdateV1(doc4, state1, nil))

	t2 := doc2.GetText("content")
	t3 := doc3.GetText("content")
	t4 := doc4.GetText("content")
	doc2.Transact(func(txn *Transaction) { t2.Insert(txn, 1, "X", nil) }) // between a and b
	doc3.Transact(func(txn *Transaction) { t3.Insert(txn, 2, "Y", nil) }) // between b and c
	doc4.Transact(func(txn *Transaction) { t4.Insert(txn, 3, "Z", nil) }) // between c and d

	// Cross-apply all updates everywhere.
	u2 := EncodeStateAsUpdateV1(doc2, doc1.StateVector())
	u3 := EncodeStateAsUpdateV1(doc3, doc1.StateVector())
	u4 := EncodeStateAsUpdateV1(doc4, doc1.StateVector())
	for _, d := range []*Doc{doc1, doc2, doc3, doc4} {
		for _, u := range [][]byte{u2, u3, u4} {
			// Skip self-updates (would be no-ops anyway).
			require.NoError(t, ApplyUpdateV1(d, u, nil))
		}
	}

	got1, got2, got3, got4 := t1.ToString(), t2.ToString(), t3.ToString(), t4.ToString()
	assert.Equal(t, got1, got2, "convergence required (1↔2)")
	assert.Equal(t, got1, got3, "convergence required (1↔3)")
	assert.Equal(t, got1, got4, "convergence required (1↔4)")
	assert.Equal(t, "aXbYcZd", got1, "each OriginRight must be respected")
}

// Scenario 2 (audit): The right-origin target is already tombstoned. The
// conflict-scan loop must still terminate at the tombstone position rather
// than walking past it.
func TestYATA_OriginRight_DeletedRightNeighbour(t *testing.T) {
	// Seed: "ab" on doc1.
	doc1 := newTestDoc(1)
	t1 := doc1.GetText("content")
	doc1.Transact(func(txn *Transaction) { t1.Insert(txn, 0, "ab", nil) })

	// Sync to doc2 and doc3.
	doc2 := New(WithClientID(2))
	doc3 := New(WithClientID(3))
	state1 := EncodeStateAsUpdateV1(doc1, nil)
	require.NoError(t, ApplyUpdateV1(doc2, state1, nil))
	require.NoError(t, ApplyUpdateV1(doc3, state1, nil))

	// doc2 inserts "X" between a and b → "aXb" (OriginRight=b).
	t2 := doc2.GetText("content")
	doc2.Transact(func(txn *Transaction) { t2.Insert(txn, 1, "X", nil) })

	// doc3 (concurrent with doc2's insert) deletes "b" → "a".
	t3 := doc3.GetText("content")
	doc3.Transact(func(txn *Transaction) { t3.Delete(txn, 1, 1) })

	// Cross-apply.
	u2 := EncodeStateAsUpdateV1(doc2, doc1.StateVector())
	u3 := EncodeStateAsUpdateV1(doc3, doc1.StateVector())
	require.NoError(t, ApplyUpdateV1(doc1, u2, nil))
	require.NoError(t, ApplyUpdateV1(doc1, u3, nil))
	require.NoError(t, ApplyUpdateV1(doc2, u3, nil))
	require.NoError(t, ApplyUpdateV1(doc3, u2, nil))

	got1, got2, got3 := t1.ToString(), t2.ToString(), t3.ToString()
	assert.Equal(t, got1, got2, "convergence required (1↔2)")
	assert.Equal(t, got1, got3, "convergence required (1↔3)")
	// X stays between a and b's tombstone position; b is gone. Result: "aX".
	assert.Equal(t, "aX", got1, "OriginRight must still bound the scan when right neighbour is deleted")
}
