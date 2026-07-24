// Package websocket - benchmark for issue #182 (G3): proves room load no
// longer serializes under the global rooms lock (s.rmu). Task 9 moved
// LoadDoc / decode / OnLoadDocument off s.rmu behind a per-room `ready`
// barrier (see server.go's getOrCreateRoom / createRoomPlaceholder / loadRoom
// and the correctness regressions in server_load_test.go). This file adds the
// missing performance proof: with a fixed-latency LoadDoc, N concurrent
// connects to N DISTINCT rooms should complete in wall-clock time close to
// one load's latency (parallel), not N times that latency (serialized).
package websocket

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fixedLatencyAdapter is a PersistenceAdapter whose LoadDoc sleeps a fixed
// latency before returning empty state (no stored data), uniformly modeling
// a slow persistence backend (network round trip, disk seek, cold cache) for
// every room. StoreUpdate is a no-op; this benchmark never writes.
type fixedLatencyAdapter struct {
	latency time.Duration
}

func (a fixedLatencyAdapter) LoadDoc(string) ([]byte, error) {
	time.Sleep(a.latency)
	return nil, nil
}

func (a fixedLatencyAdapter) StoreUpdate(string, []byte) error { return nil }

// loadNDistinctRooms starts n rooms loading concurrently against s (each name
// unique) and blocks until every getOrCreateRoom call has returned, failing
// tb immediately on any error. It returns the elapsed wall-clock time.
func loadNDistinctRooms(tb testing.TB, s *Server, n int, prefix string) time.Duration {
	tb.Helper()
	var wg sync.WaitGroup
	wg.Add(n)
	start := time.Now()
	for j := 0; j < n; j++ {
		room := fmt.Sprintf("%s-%d", prefix, j)
		go func() {
			defer wg.Done()
			if _, err := s.getOrCreateRoom(context.Background(), room); err != nil {
				tb.Errorf("getOrCreateRoom(%s): %v", room, err)
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

// BenchmarkGetOrCreateRoom_ConcurrentDistinctRoomLoads is the #182 (G3)
// headline performance benchmark. Each op loads roomCount DISTINCT rooms
// concurrently against an adapter whose LoadDoc sleeps loadLatency; b.N (or
// -benchtime=Nx) controls how many times that whole batch repeats.
//
// Pre-fix (load under s.rmu): a batch takes ~roomCount*loadLatency, because
// every room's LoadDoc serializes behind the single global lock.
// Post-fix (Task 9's per-room ready barrier, load off s.rmu): a batch takes
// ~loadLatency, because distinct rooms load in parallel.
//
// Run:
//
//	go test ./provider/websocket/ -bench 'Load|RoomCreate' -run '^$' -benchtime=5x
//
// Read ns/op against roomCount*loadLatency (serialized) vs loadLatency
// (parallel) to judge which regime the code is in.
func BenchmarkGetOrCreateRoom_ConcurrentDistinctRoomLoads(b *testing.B) {
	const (
		roomCount   = 20
		loadLatency = 20 * time.Millisecond
	)
	adapter := fixedLatencyAdapter{latency: loadLatency}

	for i := 0; i < b.N; i++ {
		s := NewServerWithPersistence(adapter)
		loadNDistinctRooms(b, s, roomCount, fmt.Sprintf("room-%d", i))
	}
}

// BenchmarkReconnectReuse_WarmIdleRoom measures the #183 (G4) reconnect-reuse
// fast path: with RoomIdleTimeout > 0 an idle room stays resident, so a
// reconnect to it hits the warm in-memory doc and pays NONE of the adapter's
// LoadDoc latency. Each op re-resolves the SAME already-resident room, so the
// steady-state cost is a map lookup plus the ready-barrier receive — orders of
// magnitude below the loadLatency a cold (evicted) room would re-incur. Compare
// ns/op against loadLatency to confirm the reuse path avoids the reload.
//
// Run:
//
//	go test ./provider/websocket/ -bench 'ReconnectReuse' -run '^$' -benchtime=1000x
func BenchmarkReconnectReuse_WarmIdleRoom(b *testing.B) {
	const loadLatency = 20 * time.Millisecond
	s := NewServerWithPersistence(fixedLatencyAdapter{latency: loadLatency})
	s.RoomIdleTimeout = time.Hour // keep the room resident for the whole run
	// Prime the room once (pays the one-time LoadDoc latency).
	if _, err := s.getOrCreateRoom(context.Background(), "warm"); err != nil {
		b.Fatalf("prime getOrCreateRoom: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.getOrCreateRoom(context.Background(), "warm"); err != nil {
			b.Fatalf("warm getOrCreateRoom: %v", err)
		}
	}
	b.StopTimer()
	_ = s.Shutdown(context.Background())
}

// TestGetOrCreateRoom_ConcurrentDistinctRooms_LoadIsParallel is a timed-test
// companion to the benchmark above, meant to run under plain `go test` (no
// -bench flag) so the #182 (G3) parallelism guarantee is checked on every CI
// run, not just when someone remembers to pass -bench.
//
// It loads roomCount distinct rooms concurrently against a fixed-latency
// LoadDoc and asserts the whole batch finishes well under
// serializedBound (< roomCount*loadLatency): serialized loading (the pre-fix
// behavior, room load running under s.rmu) would take roughly
// roomCount*loadLatency = 300ms here; parallel loading (post-fix, Task 9's
// per-room ready barrier) takes roughly loadLatency = 30ms. The bound is set
// generously (3x a single load's latency) to absorb scheduler/CI jitter while
// remaining decisively below the serialized total, so this fails loudly on a
// regression back to lock-held loads without flaking on a merely slow CI box.
func TestGetOrCreateRoom_ConcurrentDistinctRooms_LoadIsParallel(t *testing.T) {
	const (
		roomCount       = 10
		loadLatency     = 30 * time.Millisecond
		serializedBound = 3 * loadLatency // generous: well below roomCount*loadLatency (300ms), well above one load (30ms)
	)
	adapter := fixedLatencyAdapter{latency: loadLatency}
	s := NewServerWithPersistence(adapter)

	elapsed := loadNDistinctRooms(t, s, roomCount, "room")

	t.Logf("%d distinct rooms x %s LoadDoc: %s wall-clock (serialized would be ~%s)",
		roomCount, loadLatency, elapsed, roomCount*loadLatency)

	if elapsed >= serializedBound {
		t.Fatalf("loading %d distinct rooms took %s, want < %s (roughly one load's latency, not %d x latency = %s) — room load may be serializing under the global lock again (#182)",
			roomCount, elapsed, serializedBound, roomCount, roomCount*loadLatency)
	}
}
