package crdt

import (
	"fmt"
	"testing"
)

func BenchmarkMapNewKeysAfterDifferentClient(b *testing.B) {
	base := New(WithClientID(1))
	m := base.GetMap("map")
	base.Transact(func(txn *Transaction) {
		for i := 0; i < 50000; i++ {
			m.Set(txn, fmt.Sprint(i), true)
		}
	})
	update := EncodeStateAsUpdateV1(base, nil)
	base.Destroy()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		d := New(WithClientID(2))
		if err := ApplyUpdateV1(d, update, nil); err != nil {
			b.Fatal(err)
		}
		m := d.GetMap("map")
		b.StartTimer()
		d.Transact(func(txn *Transaction) {
			for i := 0; i < 1000; i++ {
				m.Set(txn, fmt.Sprintf("new-%d", i), true)
			}
		})
		b.StopTimer()
		d.Destroy()
	}
}
