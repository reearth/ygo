package crdt_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/reearth/ygo/testutil/fuzz"
)

// fuzzIter is the number of seeds TestFuzzConvergence sweeps. Defaults to 1000
// (the CI set); override for a heavy soak run, e.g.
//
//	FUZZ_ITER=50000 go test ./crdt/ -run TestFuzzConvergence -timeout 30m
func fuzzIter() uint64 {
	if v := os.Getenv("FUZZ_ITER"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 1000
}

// TestFuzzConvergence runs the fixed CI seed set; deterministic, no node.
func TestFuzzConvergence(t *testing.T) {
	n := fuzzIter()
	for seed := uint64(0); seed < n; seed++ {
		peers, err := fuzz.RunGo(fuzz.Generate(seed))
		if err != nil {
			t.Fatalf("seed %d: %v (reproduce: FUZZ_SEED=%d)", seed, err, seed)
		}
		if err := fuzz.Converged(peers); err != nil {
			t.Fatalf("seed %d: %v (reproduce: FUZZ_SEED=%d)", seed, err, seed)
		}
	}
}

// TestFuzzConvergenceMoves sweeps move-enabled scenarios (moves interleaved with
// insert/delete/push/map/xml across 3–5 peers with random sync) through the
// node-free multi-peer Converged oracle. Moves are an ygo wire extension yjs
// cannot decode (ref 11), so they are validated here by ygo-internal convergence
// rather than the yjs cross-impl oracle. Both bugs caught in #190 review
// (move+delete mis-delete; insert-beyond-end into a moved array) are of exactly
// this class.
func TestFuzzConvergenceMoves(t *testing.T) {
	n := fuzzIter()
	for seed := uint64(0); seed < n; seed++ {
		peers, err := fuzz.RunGo(fuzz.GenerateWith(seed, fuzz.GenOpts{Moves: true}))
		if err != nil {
			t.Fatalf("seed %d: %v (reproduce: FUZZ_SEED=%d, moves)", seed, err, seed)
		}
		if err := fuzz.Converged(peers); err != nil {
			t.Fatalf("seed %d: %v (reproduce: FUZZ_SEED=%d, moves)", seed, err, seed)
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
		if os.Getenv("YGO_REQUIRE_NODE") == "1" {
			t.Fatal("YGO_REQUIRE_NODE=1 but fuzz cross-impl oracle could not run (node/yjs unavailable)")
		}
		t.Skip("node/yjs unavailable — skipping cross-impl oracle")
	}
	for i, s := range scen {
		if err := fuzz.CrossImplEqual(s, results[i]); err != nil {
			t.Fatalf("seed %d: %v (reproduce: FUZZ_SEED=%d)", s.Seed, err, s.Seed)
		}
	}
}

// TestFuzzCorpus replays every frozen scenario under testutil/fuzz/corpus,
// each a minimized reproducer for a bug that once diverged. Node-free.
func TestFuzzCorpus(t *testing.T) {
	scen, err := fuzz.LoadCorpus("../testutil/fuzz/corpus")
	if err != nil {
		t.Fatal(err)
	}
	if len(scen) == 0 {
		t.Fatal("corpus is empty")
	}
	for _, s := range scen {
		peers, err := fuzz.RunGo(s)
		if err != nil {
			t.Fatalf("corpus seed %d: %v", s.Seed, err)
		}
		if err := fuzz.Converged(peers); err != nil {
			t.Fatalf("corpus seed %d: %v", s.Seed, err)
		}
	}
}

// TestFuzzSeed replays a single generated scenario for debugging. Set
// FUZZ_SEED=<n> (the value printed by a TestFuzzConvergence failure).
func TestFuzzSeed(t *testing.T) {
	sv := os.Getenv("FUZZ_SEED")
	if sv == "" {
		t.Skip("set FUZZ_SEED=<n> to replay one scenario")
	}
	n, err := strconv.ParseUint(sv, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	peers, err := fuzz.RunGo(fuzz.Generate(n))
	if err != nil {
		t.Fatalf("seed %d: %v", n, err)
	}
	if err := fuzz.Converged(peers); err != nil {
		t.Fatalf("seed %d: %v", n, err)
	}
}
