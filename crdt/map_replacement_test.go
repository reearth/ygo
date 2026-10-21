package crdt

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_MapReplacement_ConcurrentAndUnrelatedKeys(t *testing.T) {
	source := newTestDoc(1)
	defer source.Destroy()
	values := source.GetMap("values")
	source.Transact(func(txn *Transaction) {
		for i := 0; i < 20; i++ {
			values.Set(txn, fmt.Sprint(i), "initial")
		}
	})
	initial := EncodeStateAsUpdateV1(source, nil)
	updates := make([][]byte, 0, 2)
	for _, client := range []uint64{2, 3} {
		peer := newTestDoc(client)
		require.NoError(t, ApplyUpdateV1(peer, initial, nil))
		values := peer.GetMap("values")
		peer.Transact(func(txn *Transaction) { values.Set(txn, "0", fmt.Sprint(client)) })
		updates = append(updates, EncodeStateAsUpdateV1(peer, source.StateVector()))
		peer.Destroy()
	}
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		peer := newTestDoc(4)
		require.NoError(t, ApplyUpdateV1(peer, initial, nil))
		for _, index := range order {
			require.NoError(t, ApplyUpdateV1(peer, updates[index], nil))
		}
		values := peer.GetMap("values")
		winner, _ := values.Get("0")
		require.Equal(t, "3", winner)
		peer.Transact(func(txn *Transaction) { values.Set(txn, "0", "latest") })
		got, _ := values.Get("0")
		require.Equal(t, "latest", got)
		for i := 1; i < 20; i++ {
			got, _ := values.Get(fmt.Sprint(i))
			require.Equal(t, "initial", got)
		}
		peer.Destroy()
	}
}

func BenchmarkCheckpointMapReplacements(b *testing.B) {
	doc := newTestDoc(1)
	defer doc.Destroy()
	values := doc.GetMap("values")
	for round := 0; round < 2; round++ {
		doc.Transact(func(txn *Transaction) {
			for i := 0; i < 1000; i++ {
				values.Set(txn, fmt.Sprint(i), round)
			}
		})
	}
	update := EncodeStateAsUpdateV1(doc, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		receiver := New()
		if err := ApplyUpdateV1(receiver, update, nil); err != nil {
			b.Fatal(err)
		}
		receiver.Destroy()
	}
}
