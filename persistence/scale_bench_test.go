//go:build benchheavy

// Package persistence benchmarks for issue #180 (Task 5): heavy-tier
// persistence scale benchmarks.
//
//   - BenchmarkMemoryPersistence_FlushVsDocSize measures the cost of
//     LoadDoc (materialize) as the number of stored single-op V1 updates for
//     a room grows — surfacing the MergeUpdatesV1-over-all-blobs cost that a
//     LegacyAdapter-backed VersionedPersistence pays on every load when no
//     compaction has run.
//   - BenchmarkPersistThroughput_Coalescing measures end-to-end update
//     throughput through a websocket Server with default persistence
//     coalescing enabled, and reports the resulting coalesce-hit-rate (the
//     fraction of updates absorbed into a batch rather than persisted
//     individually) via a counting PersistenceAdapter shim — no production
//     code is touched or instrumented.
//
// Run:
//
//	go test -tags benchheavy ./persistence/ -run '^$' -bench 'FlushVsDocSize|Coalescing' -benchtime=1x -benchmem
package persistence_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// ── BenchmarkMemoryPersistence_FlushVsDocSize ──────────────────────────────

// buildSingleOpUpdates builds n single-character-append V1 updates from one
// growing doc (client 1), each update carrying only the diff introduced by
// its own transaction (via doc.StateVector() taken immediately before the
// transact). This mirrors how a real editor streams incremental updates to a
// PersistenceAdapter's StoreUpdate — one call per committed transaction —
// rather than n copies of the full document state.
func buildSingleOpUpdates(n int) [][]byte {
	doc := crdt.New(crdt.WithClientID(1))
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

// BenchmarkMemoryPersistence_FlushVsDocSize times LoadDoc (the materialize
// path: MergeUpdatesV1 folding every stored blob into one head) against a
// MemoryPersistence room seeded with N single-op updates via a LegacyAdapter.
// The N updates are built and stored OUTSIDE the timer; only LoadDoc is
// timed, isolating the per-load merge cost as N grows.
//
// Sizes are bounded to {1_000, 10_000, 100_000}: 100_000 was measured at
// well under the ~3s/~1GB CI-safety ceiling noted in the task brief (see
// task-5-report.md), so no reduction was needed.
func BenchmarkMemoryPersistence_FlushVsDocSize(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		n := n
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			updates := buildSingleOpUpdates(n)

			store := persistence.NewMemoryPersistence()
			ad := persistence.NewLegacyAdapter(store)
			const room = "room"
			for _, upd := range updates {
				if err := ad.StoreUpdate(room, upd); err != nil {
					b.Fatalf("StoreUpdate: %v", err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ad.LoadDoc(room); err != nil {
					b.Fatalf("LoadDoc: %v", err)
				}
			}
		})
	}
}

// ── BenchmarkPersistThroughput_Coalescing ──────────────────────────────────

// countingAdapter wraps a ygws.PersistenceAdapter and atomically counts every
// StoreUpdate call, so the coalesce-hit-rate below can be computed purely
// from an external observation point (the adapter boundary) rather than by
// reading any Server-internal state.
type countingAdapter struct {
	inner  ygws.PersistenceAdapter
	stores int64 // atomic
}

func (c *countingAdapter) LoadDoc(room string) ([]byte, error) { return c.inner.LoadDoc(room) }

func (c *countingAdapter) StoreUpdate(room string, u []byte) error {
	atomic.AddInt64(&c.stores, 1)
	return c.inner.StoreUpdate(room, u)
}

// persistThroughputM is the number of updates driven through the server per
// benchmark iteration. Kept modest so the workload (M sequential WS sends
// plus one flush) stays bounded under -benchtime=1x.
const persistThroughputM = 1000

// persistThroughputCoalesceWindow is a short debounce window (vs. the 2s
// production default) so the benchmark's forced flush at the end of each
// iteration has an actual batch to drain instead of racing a multi-second
// timer.
const persistThroughputCoalesceWindow = 50 * time.Millisecond

// BenchmarkPersistThroughput_Coalescing drives persistThroughputM sequential
// updates from one websocket peer into a room backed by a countingAdapter,
// waits for the server to have received and applied all of them, then forces
// the pending coalesced batch to flush (Server.CloseRoom drains
// flush-before-evict) and reports:
//
//   - "updates/s": persistThroughputM divided by the elapsed time from the
//     first send through the forced flush completing.
//   - "coalesce-hit-rate": 1 - (StoreUpdate calls observed / updates sent),
//     i.e. the fraction of updates that were absorbed into a coalesced batch
//     rather than causing their own persistence write. Computed purely from
//     the countingAdapter's counter, never from Server internals.
func BenchmarkPersistThroughput_Coalescing(b *testing.B) {
	b.Run(fmt.Sprintf("M=%d", persistThroughputM), func(b *testing.B) {
		var totalElapsed time.Duration
		var totalStores int64
		for i := 0; i < b.N; i++ {
			elapsed, stores := runPersistThroughputIteration(b)
			totalElapsed += elapsed
			totalStores += stores
		}
		// Reported once for the whole run (b.N iterations): testing.B keeps
		// only the last value passed to ReportMetric per unit name, so
		// calling it per-iteration (as a prior version of this benchmark
		// did) silently discarded every iteration but the last. Aggregating
		// totals across all b.N iterations first and reporting once here
		// makes both metrics reflect the full run.
		b.ReportMetric(float64(persistThroughputM*b.N)/totalElapsed.Seconds(), "updates/s")
		b.ReportMetric(1-float64(totalStores)/float64(persistThroughputM*b.N), "coalesce-hit-rate")
	})
}

// runPersistThroughputIteration runs one iteration of the throughput
// workload and returns the elapsed time (send-through-flush) and the number
// of StoreUpdate calls observed, for the caller to aggregate across all b.N
// iterations before reporting metrics once.
func runPersistThroughputIteration(b *testing.B) (elapsed time.Duration, stores int64) {
	b.Helper()

	counting := &countingAdapter{inner: persistence.NewLegacyAdapter(persistence.NewMemoryPersistence())}
	s := ygws.NewServerWithPersistence(counting)
	s.PersistCoalesceWindow = persistThroughputCoalesceWindow
	defer func() { _ = s.Shutdown(context.Background()) }()
	ts := httptest.NewServer(s)
	defer ts.Close()

	const room = "room"
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("t")

	conn := dialWSBench(b, ts, room)
	defer func() { _ = conn.Close() }()
	drainHandshakeBench(b, conn)

	start := time.Now()
	for j := 0; j < persistThroughputM; j++ {
		sv := doc.StateVector()
		doc.Transact(func(txn *crdt.Transaction) {
			txt.Insert(txn, txt.Len(), "a", nil)
		})
		sendV1UpdateBench(b, conn, crdt.EncodeStateAsUpdateV1(doc, sv))
	}

	// Confirm the server has actually received and applied every update
	// (over a real loopback socket, delivery to the room's doc is
	// asynchronous relative to WriteMessage returning) before forcing the
	// flush and reading the counter — otherwise a straggling in-flight
	// update could still be pending when CloseRoom evicts the room.
	waitForDocTextLen(b, s, room, persistThroughputM, 5*time.Second)

	// Force the pending coalesced batch to flush while the room is still
	// discoverable (Server.CloseRoom's flush-before-evict guarantee — see
	// TestPersistTeardown_CloseRoomFlushesBeforeEvict in
	// provider/websocket/persistence_coalesce_test.go). force=true because a
	// peer is still connected.
	if err := s.CloseRoom(room, true); err != nil {
		b.Fatalf("CloseRoom: %v", err)
	}
	elapsed = time.Since(start)
	stores = atomic.LoadInt64(&counting.stores)
	return elapsed, stores
}

// waitForDocTextLen polls the server's live, in-memory doc (an exported
// Server.GetDoc — not an internal field) until the room's "t" text reaches
// the expected length or timeout elapses. Used only to synchronize the
// benchmark before it forces a flush; the coalesce-hit-rate metric itself is
// still computed solely from the countingAdapter's counter.
func waitForDocTextLen(b *testing.B, s *ygws.Server, room string, want int, timeout time.Duration) {
	b.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if doc := s.GetDoc(room); doc != nil {
			if doc.GetText("t").Len() >= want {
				return
			}
		}
		if time.Now().After(deadline) {
			b.Fatalf("timed out waiting for room %q text len >= %d", room, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ── minimal WS benchmark helpers ────────────────────────────────────────
//
// adapter_test.go already defines dialWS/drainWS/sendV1Update in this same
// package, but those take *testing.T (via testify's require) and can't be
// reused from a *testing.B context. These *Bench variants are self-contained
// (b.Fatalf instead of require) and named to avoid colliding with the
// non-benchheavy helpers when both files compile together under -tags
// benchheavy.

func dialWSBench(b *testing.B, ts *httptest.Server, room string) *gws.Conn {
	b.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + room
	conn, _, err := gws.DefaultDialer.Dial(url, nil)
	if err != nil {
		b.Fatalf("dial %s: %v", room, err)
	}
	return conn
}

func drainHandshakeBench(b *testing.B, conn *gws.Conn) {
	b.Helper()
	for i := 0; i < 3; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil {
			b.Fatalf("drain handshake frame %d: %v", i, err)
		}
	}
}

func sendV1UpdateBench(b *testing.B, conn *gws.Conn, update []byte) {
	b.Helper()
	inner := encoding.NewEncoder()
	inner.WriteVarUint(ygsync.MsgUpdate)
	inner.WriteVarBytes(update)
	outer := encoding.NewEncoder()
	outer.WriteVarUint(0) // msgSync
	outer.WriteRaw(inner.Bytes())
	if err := conn.WriteMessage(gws.BinaryMessage, outer.Bytes()); err != nil {
		b.Fatalf("write update: %v", err)
	}
}
