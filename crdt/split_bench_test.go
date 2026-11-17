package crdt

import (
	"strconv"
	"testing"
)

// BenchmarkSplitUpdateV1_ChainedClients splits the state of a document whose
// structs chain across clients (chainedClientsUpdate), at four room sizes, so
// the time per split shows how the walk scales with the number of clients.
func BenchmarkSplitUpdateV1_ChainedClients(b *testing.B) {
	for _, clients := range []int{1_000, 2_000, 4_000, 8_000} {
		b.Run(strconv.Itoa(clients), func(b *testing.B) {
			update := chainedClientsUpdate(b, clients)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := SplitUpdateV1(update, nil, 64<<10); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSplitStateAsUpdateV1_ChainedClients cuts the same rooms from the
// document's own structs, as the answer to a SyncStep1 does.
func BenchmarkSplitStateAsUpdateV1_ChainedClients(b *testing.B) {
	for _, clients := range []int{1_000, 2_000, 4_000, 8_000} {
		b.Run(strconv.Itoa(clients), func(b *testing.B) {
			doc := New(WithClientID(1))
			if err := ApplyUpdateV1(doc, chainedClientsUpdate(b, clients), nil); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := SplitStateAsUpdateV1(doc, nil, 64<<10); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
