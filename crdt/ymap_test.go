package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ── YMap ──────────────────────────────────────────────────────────────────────

func TestUnit_YMap_SetGet(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")

	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "name", "Alice")
		m.Set(txn, "age", 30)
	})

	v, ok := m.Get("name")
	assert.True(t, ok)
	assert.Equal(t, "Alice", v)

	v, ok = m.Get("age")
	assert.True(t, ok)
	assert.Equal(t, 30, v)
}

func TestUnit_YMap_Get_Missing(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")

	v, ok := m.Get("nope")
	assert.False(t, ok)
	assert.Nil(t, v)
}

func TestUnit_YMap_Has(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")

	doc.Transact(func(txn *Transaction) { m.Set(txn, "x", 1) })

	assert.True(t, m.Has("x"))
	assert.False(t, m.Has("y"))
}

func TestUnit_YMap_Delete(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")

	doc.Transact(func(txn *Transaction) { m.Set(txn, "x", 1) })
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "x") })

	assert.False(t, m.Has("x"))
	_, ok := m.Get("x")
	assert.False(t, ok)
}

func TestUnit_YMap_OverwriteSameKey(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")

	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", "v1") })
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", "v2") })

	v, ok := m.Get("k")
	assert.True(t, ok)
	assert.Equal(t, "v2", v)
}

func TestUnit_YMap_MultipleKeys(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")

	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "name", "Alice")
		m.Set(txn, "age", 30)
		m.Set(txn, "active", true)
	})

	assert.Equal(t, 3, len(m.Keys()))

	entries := m.Entries()
	assert.Equal(t, "Alice", entries["name"])
	assert.Equal(t, 30, entries["age"])
	assert.Equal(t, true, entries["active"])
}

func TestUnit_YMap_Keys_ExcludesDeleted(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")

	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "a", 1)
		m.Set(txn, "b", 2)
	})
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "a") })

	keys := m.Keys()
	assert.Len(t, keys, 1)
	assert.Equal(t, "b", keys[0])
}

func TestUnit_YMap_Observe_KeysChanged(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")
	var lastEvent *YMapEvent

	m.Observe(func(e YMapEvent) { lastEvent = &e })

	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "x", 1)
		m.Set(txn, "y", 2)
	})

	assert.NotNil(t, lastEvent)
	assert.Contains(t, lastEvent.KeysChanged, "x")
	assert.Contains(t, lastEvent.KeysChanged, "y")
}

func TestUnit_YMap_Observe_Unsubscribe(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("data")
	calls := 0
	unsub := m.Observe(func(_ YMapEvent) { calls++ })

	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", 1) })
	unsub()
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", 2) })

	assert.Equal(t, 1, calls)
}

// ── YMap integration ──────────────────────────────────────────────────────────

func TestInteg_YMap_ConcurrentSet_HigherClientIDWins(t *testing.T) {
	// Both clients set the same key concurrently. Higher ClientID must win.
	doc1 := newTestDoc(1)
	doc2 := newTestDoc(2)

	m1 := doc1.GetMap("data")
	m2 := doc2.GetMap("data")

	doc1.Transact(func(txn *Transaction) { m1.Set(txn, "key", "from-client1") })
	doc2.Transact(func(txn *Transaction) { m2.Set(txn, "key", "from-client2") })

	// Apply doc1's item into doc2.
	doc1.store.IterateFrom(doc2.store.StateVector(), func(item *Item) {
		doc2.Transact(func(txn *Transaction) {
			clone := &Item{
				ID:          item.ID,
				Origin:      item.Origin,
				OriginRight: item.OriginRight,
				Parent:      &m2.abstractType,
				ParentSub:   item.ParentSub,
				Content:     item.Content.Copy(),
			}
			clone.integrate(txn, 0)
		})
	})

	// Apply doc2's item into doc1.
	doc2.store.IterateFrom(doc1.store.StateVector(), func(item *Item) {
		doc1.Transact(func(txn *Transaction) {
			clone := &Item{
				ID:          item.ID,
				Origin:      item.Origin,
				OriginRight: item.OriginRight,
				Parent:      &m1.abstractType,
				ParentSub:   item.ParentSub,
				Content:     item.Content.Copy(),
			}
			clone.integrate(txn, 0)
		})
	})

	v1, _ := m1.Get("key")
	v2, _ := m2.Get("key")
	assert.Equal(t, v1, v2, "both peers must converge to the same value")
	assert.Equal(t, "from-client2", v1, "higher clientID (2) must win")
}

func TestInteg_YMap_ConcurrentSet_Convergent(t *testing.T) {
	// Apply in both orders; results must match.
	apply := func(winnerFirst bool) any {
		doc := newTestDoc(99)
		m := doc.GetMap("data")

		item1 := &Item{
			ID: ID{Client: 1, Clock: 0}, ParentSub: "k",
			Content: NewContentAny("v1"), Parent: &m.abstractType,
		}
		item2 := &Item{
			ID: ID{Client: 2, Clock: 0}, ParentSub: "k",
			Content: NewContentAny("v2"), Parent: &m.abstractType,
		}

		doc.Transact(func(txn *Transaction) {
			if winnerFirst {
				item2.integrate(txn, 0)
				item1.integrate(txn, 0)
			} else {
				item1.integrate(txn, 0)
				item2.integrate(txn, 0)
			}
		})

		v, _ := m.Get("k")
		return v
	}

	assert.Equal(t, apply(true), apply(false), "concurrent map set must converge")
}
