package fuzz_test

import (
	"encoding/json"
	"testing"

	"github.com/reearth/ygo/testutil/fuzz"
)

func TestGenerate_Deterministic(t *testing.T) {
	a := fuzz.Generate(12345)
	b := fuzz.Generate(12345)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatal("same seed must produce identical scenario")
	}
	if fuzz.Generate(1).Seed != 1 || fuzz.Generate(1).NumPeers < 3 || fuzz.Generate(1).NumPeers > 5 {
		t.Fatalf("bad peers: %d", fuzz.Generate(1).NumPeers)
	}
	if len(a.Steps) < 60 {
		t.Fatalf("too few steps: %d", len(a.Steps))
	}
}

func TestGenerate_MergeBias(t *testing.T) {
	// Across many seeds, Merge/Diff sync methods should dominate Apply.
	var merge, apply int
	for s := uint64(0); s < 50; s++ {
		for _, st := range fuzz.Generate(s).Steps {
			if st.Kind != fuzz.StepSync {
				continue
			}
			switch st.Method {
			case fuzz.MergeV1, fuzz.MergeV2, fuzz.DiffV1, fuzz.DiffV2:
				merge++
			case fuzz.ApplyV1, fuzz.ApplyV2:
				apply++
			}
		}
	}
	if merge <= apply {
		t.Fatalf("expected merge/diff bias, got merge=%d apply=%d", merge, apply)
	}
}

func TestGenerate_MovesAreOptIn(t *testing.T) {
	// Default generation never emits a move (yjs/cross-impl safety).
	for seed := uint64(0); seed < 200; seed++ {
		for _, st := range fuzz.Generate(seed).Steps {
			if st.Op == fuzz.OpMove {
				t.Fatalf("seed %d: default Generate emitted a move op", seed)
			}
		}
	}
}
