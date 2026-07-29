//go:build benchheavy

package websocket

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestScaleProbe is a measurement harness, not a benchmark: for each room
// count N it creates N rooms directly via getOrCreateRoom (bypassing real
// peer connections), forces a GC, and logs heap/sys memory, goroutine count,
// and RSS (Linux only, via /proc/self/status) for that size. It exists to
// spot O(rooms) growth in server-side resource usage as room count scales.
//
// This is deliberately not a pass/fail assertion on the numbers themselves —
// normal fluctuation in heap/goroutine counts across Go versions/GOMAXPROCS
// is expected and not a regression. It only fails (via t.Fatalf) on a
// genuine room-creation error.
//
// Run: go test -tags benchheavy ./provider/websocket/ -run TestScaleProbe -v
// Smoke override (smaller, single size): SCALE_PROBE_N=200 go test -tags
// benchheavy ./provider/websocket/ -run TestScaleProbe -v
func TestScaleProbe(t *testing.T) {
	sizes := []int{1000, 10000}
	if v := os.Getenv("SCALE_PROBE_N"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("SCALE_PROBE_N=%q must be a positive integer", v)
		}
		sizes = []int{n}
	}

	ctx := context.Background()
	t.Logf("%-8s %-13s %-8s %-10s %s", "rooms", "heapAllocMiB", "sysMiB", "goroutines", "rss")
	for _, n := range sizes {
		s := NewServer()
		for i := 0; i < n; i++ {
			if _, _, err := s.getOrCreateRoom(ctx, fmt.Sprintf("room-%d", i)); err != nil {
				t.Fatalf("room %d: %v", i, err)
			}
		}

		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		t.Logf("%-8d %-13d %-8d %-10d %s",
			n, m.HeapAlloc>>20, m.Sys>>20, runtime.NumGoroutine(), readRSS())

		// Isolate this size from the next one: Shutdown releases the server's
		// peer/relay/persistence goroutines (there are none here, since
		// getOrCreateRoom bypasses real peer connections and this server has no
		// persistence/relay configured), and letting s go out of scope at the
		// end of this iteration plus a forced GC reclaims the rooms/docs
		// themselves, so consecutive sizes don't accumulate memory or
		// goroutines from prior iterations.
		if err := s.Shutdown(ctx); err != nil {
			t.Logf("shutdown (size %d): %v", n, err)
		}
		runtime.GC()
	}
}

// readRSS returns the process's resident set size (VmRSS) from
// /proc/self/status on Linux, or "n/a" if that file doesn't exist (e.g. on
// macOS) or the VmRSS line can't be found/parsed.
func readRSS() string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "n/a"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return "n/a"
		}
		return strings.Join(fields[1:], "")
	}
	return "n/a"
}
