package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Stack items replay clients in first-delete order (Yjs DeleteSet Map
// order), across Merge and clone.
func TestUnit_DeleteSet_OrderedClients(t *testing.T) {
	a := newOrderedDeleteSet()
	a.add(ID{Client: 9, Clock: 0}, 1)
	a.add(ID{Client: 3, Clock: 0}, 1)
	a.add(ID{Client: 9, Clock: 5}, 1)
	require.Equal(t, []ClientID{9, 3}, a.orderedClients())

	b := newOrderedDeleteSet()
	b.add(ID{Client: 7, Clock: 0}, 1)
	b.add(ID{Client: 5, Clock: 0}, 1)
	b.add(ID{Client: 3, Clock: 2}, 1)
	a.Merge(b)
	require.Equal(t, []ClientID{9, 3, 7, 5}, a.orderedClients())
	c := cloneDeleteSet(a)
	require.Equal(t, []ClientID{9, 3, 7, 5}, c.orderedClients())

	// Decoded sets carry no order: ascending after any ordered clients.
	a.clients[4] = []DeleteRange{{Clock: 0, Len: 1}}
	a.clients[1] = []DeleteRange{{Clock: 0, Len: 1}}
	require.Equal(t, []ClientID{9, 3, 7, 5, 1, 4}, a.orderedClients())
}

// Only transaction delete sets record order: a snapshot's set is built from
// the store's map, and a recorded order would make equal snapshots differ.
func TestUnit_DeleteSet_SnapshotsCarryNoOrder(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	for c := 2; c < 12; c++ {
		peer := newTestDoc(uint64(c))
		pa := peer.GetArray("a")
		peer.Transact(func(txn *Transaction) { pa.Push(txn, []any{c}) })
		syncTo(t, peer, doc)
	}
	doc.Transact(func(txn *Transaction) { arr.Delete(txn, 0, arr.Len()) })
	first := CaptureSnapshot(doc)
	for range 20 {
		require.Equal(t, first, CaptureSnapshot(doc))
	}
}
