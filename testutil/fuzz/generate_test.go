package fuzz

import (
	"encoding/json"
	"testing"
)

func TestGenerate_Deterministic(t *testing.T) {
	a := Generate(12345)
	b := Generate(12345)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatal("same seed must produce identical scenario")
	}
	if Generate(1).Seed != 1 || Generate(1).NumPeers < 3 || Generate(1).NumPeers > 5 {
		t.Fatalf("bad peers: %d", Generate(1).NumPeers)
	}
	if len(a.Steps) < 60 {
		t.Fatalf("too few steps: %d", len(a.Steps))
	}
}

func TestGenerate_MergeBias(t *testing.T) {
	// Across many seeds, Merge/Diff sync methods should dominate Apply.
	var merge, apply int
	for s := uint64(0); s < 50; s++ {
		for _, st := range Generate(s).Steps {
			if st.Kind != StepSync {
				continue
			}
			switch st.Method {
			case MergeV1, MergeV2, DiffV1, DiffV2:
				merge++
			case ApplyV1, ApplyV2:
				apply++
			}
		}
	}
	if merge <= apply {
		t.Fatalf("expected merge/diff bias, got merge=%d apply=%d", merge, apply)
	}
}
