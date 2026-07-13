package fuzz

import "testing"

// The predicate "fails" iff a step with StrVal=="BUG" remains.
func TestShrink_RemovesIrrelevantSteps(t *testing.T) {
	s := Scenario{Seed: 1, NumPeers: 3}
	for i := 0; i < 20; i++ {
		s.Steps = append(s.Steps, Step{Kind: StepLocalOp, Root: "t", TypeKind: KindText, Op: OpInsert, StrVal: "x"})
	}
	s.Steps[10].StrVal = "BUG"
	fails := func(sc Scenario) bool {
		for _, st := range sc.Steps {
			if st.StrVal == "BUG" {
				return true
			}
		}
		return false
	}
	min := Shrink(s, fails)
	if !fails(min) {
		t.Fatal("shrunk scenario must still fail")
	}
	if len(min.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(min.Steps))
	}
}
