package fuzz

import (
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestApplyLocalOp_TextInsertClamped(t *testing.T) {
	p := &peerState{doc: crdt.New(crdt.WithClientID(1))}
	applyLocalOp(p, Step{Kind: StepLocalOp, Peer: 0, Root: "t", TypeKind: KindText, Op: OpInsert, PosHint: 999, StrVal: "ab"})
	applyLocalOp(p, Step{Kind: StepLocalOp, Peer: 0, Root: "t", TypeKind: KindText, Op: OpInsert, PosHint: 1, StrVal: "X"})
	if got := p.doc.GetText("t").ToString(); got != "aXb" {
		t.Fatalf("got %q, want aXb", got)
	}
}

func TestClampIndex(t *testing.T) {
	if clampIndex(999, 3, true) != 999%4 {
		t.Fatal("insert clamp")
	}
	if clampIndex(5, 0, false) != 0 {
		t.Fatal("empty clamp")
	}
}

func TestRunGo_Converges(t *testing.T) {
	for seed := uint64(0); seed < 200; seed++ {
		peers, err := RunGo(Generate(seed))
		if err != nil {
			t.Fatalf("seed %d: RunGo error: %v", seed, err)
		}
		if err := Converged(peers); err != nil {
			t.Fatalf("seed %d: not converged: %v", seed, err)
		}
	}
}

// TestApplySync_DoesNotMixV1AndV2Inboxes pins the peerState.inboxV1/inboxV2
// split. Before that split, a single untyped inbox let a DiffV2-staged blob
// and a later MergeV1 step land in the same MergeUpdatesV1(...) call, which
// corrupts the byte stream (V1 decoder fed V2 bytes) and returns an error
// from RunGo rather than a mere convergence mismatch. fuzz seed 0 hits this
// exact interleaving (a diffv2 step targeting peer 1's inbox followed by
// three mergev1 steps to the same peer before any drain): RunGo must return
// a nil error here regardless of whether the peers end up converged.
func TestApplySync_DoesNotMixV1AndV2Inboxes(t *testing.T) {
	if _, err := RunGo(Generate(0)); err != nil {
		t.Fatalf("RunGo returned an error (V1/V2 inbox mixing regression): %v", err)
	}
}
