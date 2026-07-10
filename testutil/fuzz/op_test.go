package fuzz

import (
	"encoding/json"
	"testing"
)

func TestScenario_JSONRoundTrip(t *testing.T) {
	s := Scenario{
		Seed: 42, NumPeers: 3,
		Steps: []Step{
			{Kind: StepLocalOp, Peer: 0, Root: "t", TypeKind: KindText, Op: OpInsert, PosHint: 5, StrVal: "hi"},
			{Kind: StepSync, From: 0, To: 1, Method: MergeV2},
			{Kind: StepGC, Peer: 1},
		},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got Scenario
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Seed != 42 || got.NumPeers != 3 || len(got.Steps) != 3 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Steps[0].StrVal != "hi" || got.Steps[1].Method != MergeV2 || got.Steps[2].Kind != StepGC {
		t.Fatalf("field mismatch: %+v", got.Steps)
	}
}
