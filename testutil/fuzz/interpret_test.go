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
