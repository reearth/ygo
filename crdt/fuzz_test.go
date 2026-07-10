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
