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

// BenchmarkWSMemoryPersistence_StoreUpdateVsDocSize times repeated
// StoreUpdate calls against a room already holding N updates. MemoryPersistence
// (provider/websocket/server.go) delegates to persistence.LegacyAdapter +
// persistence.MemoryPersistence (KeepVersions = 1): each StoreUpdate call
// APPENDS the update — O(update), not O(document) — and only folds the
// room's backlog into one blob once every CompactEvery appends (default 500,
// #186). That fold is still O(document); it is just paid once per
// CompactEvery calls instead of on every one, so per-call cost keeps growing
// with document size, only far more slowly (see this repo's godoc on
// MemoryPersistence and RELEASE_NOTES.md for the honest framing).
//
// b.N in a Go benchmark is normally far larger than CompactEvery, so the
// reported ns/op here AVERAGES many cheap O(update) appends together with
// the rare O(document) fold that lands roughly once every CompactEvery
// iterations — that averaging is why the numbers below read as close to flat
// across seed sizes; it is not evidence that any single call is cost-capped.
//
// N is seeded and APPENDED (not merged) OUTSIDE the timer (b.ResetTimer
// below); only the StoreUpdate calls inside the b.N loop are timed,
// isolating the per-call cost as N grows.
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
