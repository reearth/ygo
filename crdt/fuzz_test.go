package crdt_test

import (
	"testing"

	"github.com/reearth/ygo/testutil/fuzz"
)

// TestFuzzConvergence runs the fixed CI seed set; deterministic, no node.
func TestFuzzConvergence(t *testing.T) {
	for seed := uint64(0); seed < 1000; seed++ {
		peers, err := fuzz.RunGo(fuzz.Generate(seed))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if err := fuzz.Converged(peers); err != nil {
			t.Fatalf("seed %d: %v (reproduce: FUZZ_SEED=%d)", seed, err, seed)
		}
	}
}

// TestFuzzCrossImpl replays each scenario against real Yjs (node) and asserts
// ygo's own replay matches Yjs both logically (per-peer ToJSON/ToXML) and by
// round-trip (a fresh ygo doc that decodes Yjs's encoded update must produce
// the same logical view). Catches "ygo is self-consistent but disagrees with
// Yjs" bugs the Go-vs-Go Converged oracle cannot. Skips when node/yjs absent.
func TestFuzzCrossImpl(t *testing.T) {
	var scen []fuzz.Scenario
	for seed := uint64(0); seed < 200; seed++ {
		scen = append(scen, fuzz.Generate(seed))
	}
	results, ok := fuzz.RunNode(scen)
	if !ok {
		t.Skip("node/yjs unavailable — skipping cross-impl oracle")
	}
	for i, s := range scen {
		if err := fuzz.CrossImplEqual(s, results[i]); err != nil {
			t.Fatalf("seed %d: %v (reproduce: FUZZ_SEED=%d)", s.Seed, err, s.Seed)
		}
	}
}
