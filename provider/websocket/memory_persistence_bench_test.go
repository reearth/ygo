package websocket_test

import (
	"fmt"
	"testing"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// buildSingleOpUpdates returns n incremental V1 updates, each one single-char
// insert, mirroring persistence/scale_bench_test.go's helper of the same
// shape: the state vector is captured immediately BEFORE each transaction so
// every update carries only the diff introduced by its own commit, not a
// growing copy of the whole document.
func buildSingleOpUpdates(tb testing.TB, n int) [][]byte {
	tb.Helper()
	doc := crdt.New()
	txt := doc.GetText("t")
	updates := make([][]byte, n)
	for i := 0; i < n; i++ {
		sv := doc.StateVector()
		doc.Transact(func(txn *crdt.Transaction) {
			txt.Insert(txn, txt.Len(), "a", nil)
		})
		updates[i] = crdt.EncodeStateAsUpdateV1(doc, sv)
	}
	return updates
}

// BenchmarkWSMemoryPersistence_StoreUpdateVsDocSize times a SINGLE
// StoreUpdate against a room already holding N updates. MemoryPersistence
// (provider/websocket/server.go) folds every write into one accumulated V1
// snapshot via crdt.MergeUpdatesV1(existing, update) — merging against the
// WHOLE document on every call. Under that merge-on-write design the
// per-call cost grows with N (#186), so a session of N updates costs
// O(N²) overall even though each update is O(1)-sized. Under a future
// append-then-compact design the per-call cost must stay flat apart from
// the amortised cost of periodic compaction.
//
// N is seeded and pre-merged OUTSIDE the timer (b.ResetTimer below); only the
// single extra StoreUpdate call per b.N iteration is timed, isolating the
// per-call flush cost as N grows.
func BenchmarkWSMemoryPersistence_StoreUpdateVsDocSize(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000} {
		n := n
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			seed := buildSingleOpUpdates(b, n)
			extra := buildSingleOpUpdates(b, 1)[0]
			p := ygws.NewMemoryPersistence()
			const room = "room"
			for _, u := range seed {
				if err := p.StoreUpdate(room, u); err != nil {
					b.Fatalf("seed StoreUpdate: %v", err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := p.StoreUpdate(room, extra); err != nil {
					b.Fatalf("StoreUpdate: %v", err)
				}
			}
		})
	}
}
