package crdt

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for issue #75 — YArray.ToJSON / YMap.ToJSON must recursively unwrap
// nested ContentType values into their JSON representation. Pre-fix, nested
// shared types were silently dropped (omitted from the output entirely),
// causing silent data loss for editors, persistence layers, and snapshot
// dumps that round-trip through JSON.

// insertNestedYMap is a tiny helper that builds a nested YMap parented to
// the given YArray at the specified index, populating it inside the same
// transaction. Returns the nested map for further assertions.
func insertNestedYMap(t *testing.T, doc *Doc, arr *YArray, index int, kv map[string]any) *YMap {
	t.Helper()
	nested := &YMap{}
	nested.doc = doc
	nested.itemMap = make(map[string]*Item)
	nested.owner = nested
	doc.Transact(func(txn *Transaction) {
		at := arr.baseType()
		item := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  at,
			Content: NewContentType(&nested.abstractType),
		}
		// Position at the end for simplicity — tests can shuffle if they need
		// a specific layout.
		_ = index
		item.integrate(txn, 0)
		for k, v := range kv {
			nested.Set(txn, k, v)
		}
	})
	return nested
}

// #75 — YArray containing a nested YMap must serialise via JSON as a slice
// of plain values where the nested YMap appears as map[string]any, not as
// a *YMap pointer or as silently-dropped data.
func TestUnit_YArray_ToJSON_RecursesIntoNestedYMap(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	insertNestedYMap(t, doc, arr, 0, map[string]any{"k1": "v1", "k2": int64(42)})

	// Add some plain values alongside.
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"after"})
	})

	got := arr.ToSlice()
	require.Len(t, got, 2,
		"the nested YMap plus the plain string should produce two entries")

	nestedJSON, ok := got[0].(map[string]any)
	require.True(t, ok,
		"first entry must be the unwrapped nested YMap as map[string]any, not a raw owner pointer")
	assert.Equal(t, "v1", nestedJSON["k1"])
	assert.Equal(t, int64(42), nestedJSON["k2"])

	assert.Equal(t, "after", got[1])

	// ToJSON round-trip via json.Marshal must succeed and produce parseable JSON.
	jsonBytes, err := arr.ToJSON()
	require.NoError(t, err)
	var roundTrip []any
	require.NoError(t, json.Unmarshal(jsonBytes, &roundTrip))
	require.Len(t, roundTrip, 2)
	nestedRT, ok := roundTrip[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "v1", nestedRT["k1"])
}

// #75 cont. — YMap containing a nested YArray must serialise the nested
// array as []any in its Entries / ToJSON output.
func TestUnit_YMap_ToJSON_RecursesIntoNestedYArray(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")

	// Build a nested YArray inside the map.
	nested := &YArray{}
	nested.doc = doc
	nested.itemMap = make(map[string]*Item)
	nested.owner = nested
	doc.Transact(func(txn *Transaction) {
		// Set the nested array under key "list" via raw item placement.
		at := m.baseType()
		item := &Item{
			ID:        ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:    at,
			ParentSub: "list",
			Content:   NewContentType(&nested.abstractType),
		}
		item.integrate(txn, 0)
		nested.Push(txn, []any{int64(1), int64(2), int64(3)})

		// Plain key alongside.
		m.Set(txn, "name", "alice")
	})

	entries := m.Entries()
	assert.Equal(t, "alice", entries["name"])

	nestedSlice, ok := entries["list"].([]any)
	require.True(t, ok,
		"nested YArray under key 'list' must be unwrapped to []any, not a raw owner pointer")
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, nestedSlice)

	// JSON round-trip.
	jsonBytes, err := m.ToJSON()
	require.NoError(t, err)
	var roundTrip map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &roundTrip))
	assert.Equal(t, "alice", roundTrip["name"])
	require.Contains(t, roundTrip, "list")
}

// #75 cont. — Two-level nesting (array → map → array) recurses correctly
// all the way down. Pin the depth contract.
func TestUnit_YArray_ToJSON_DeepNesting(t *testing.T) {
	doc := newTestDoc(1)
	outer := doc.GetArray("outer")

	// outer = [ { "inner": [1, 2] } ]
	mid := &YMap{}
	mid.doc = doc
	mid.itemMap = make(map[string]*Item)
	mid.owner = mid
	inner := &YArray{}
	inner.doc = doc
	inner.itemMap = make(map[string]*Item)
	inner.owner = inner

	doc.Transact(func(txn *Transaction) {
		// Insert mid into outer.
		atOuter := outer.baseType()
		midItem := &Item{
			ID:      ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:  atOuter,
			Content: NewContentType(&mid.abstractType),
		}
		midItem.integrate(txn, 0)

		// Insert inner into mid under key "inner".
		atMid := mid.baseType()
		innerItem := &Item{
			ID:        ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:    atMid,
			ParentSub: "inner",
			Content:   NewContentType(&inner.abstractType),
		}
		innerItem.integrate(txn, 0)

		inner.Push(txn, []any{int64(1), int64(2)})
	})

	got := outer.ToSlice()
	require.Len(t, got, 1)
	midJSON, ok := got[0].(map[string]any)
	require.True(t, ok)
	innerJSON, ok := midJSON["inner"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{int64(1), int64(2)}, innerJSON)
}
