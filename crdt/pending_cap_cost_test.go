package crdt

import "testing"

// The pending cap's check before the apply costs a small update nothing: for a
// keystroke, in V1 (a comparison of its length with the room left) and in V2
// (a scan of its structs on the apply's own decoder), it allocates nothing,
// and it leaves the V2 decoder where the apply begins to read.
func TestUnit_PendingBudget_CheckBeforeApplyAllocatesNothingForAKeystroke(t *testing.T) {
	base := New(WithClientID(1))
	text := base.GetText("t")
	base.Transact(func(txn *Transaction) { text.Insert(txn, 0, "hello world", nil) })
	state := EncodeStateAsUpdateV1(base, nil)
	sv := base.StateVector()
	base.Transact(func(txn *Transaction) { text.Insert(txn, 3, "x", nil) })
	keystroke := EncodeStateAsUpdateV1(base, sv)
	keystrokeV2, err := UpdateV1ToV2(keystroke)
	if err != nil {
		t.Fatal(err)
	}
	doc := New()
	if err := ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatal(err)
	}
	docSV := doc.StateVector()

	v1 := newPendingBudget(doc, docSV, keystroke, false)
	if n := testing.AllocsPerRun(100, func() {
		if err := v1.checkBeforeApply(len(keystroke)); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("the V1 check allocated %v times", n)
	}

	dec, err := newV2Decoder(keystrokeV2)
	if err != nil {
		t.Fatal(err)
	}
	initial := *dec
	v2 := newPendingBudget(doc, docSV, keystrokeV2, true)
	v2.v2Start, v2.v2Rest = &initial, *dec.restDec
	if n := testing.AllocsPerRun(100, func() {
		if err := v2.checkBeforeApply(len(keystrokeV2)); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("the V2 check allocated %v times", n)
	}
	if start := v2.v2Rest; len(dec.restDec.RemainingBytes()) != len(start.RemainingBytes()) {
		t.Fatal("the V2 check moved the apply's decoder")
	}
	if err := ApplyUpdateV2(doc, keystrokeV2, nil); err != nil {
		t.Fatal(err)
	}
	if got := doc.GetText("t").ToString(); got != "helxlo world" {
		t.Fatalf("the text is %q", got)
	}
}
