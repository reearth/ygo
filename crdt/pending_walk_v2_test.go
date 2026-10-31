package crdt

import (
	"errors"
	"strings"
	"testing"
)

// The V2 walk reports a struct section it cannot read, and the pending-cap
// check refuses such an update for the reason the decoder gives, before the
// decoder reads it.
func TestUnit_PendingBudget_WalkStructsV2ReportsAnUnreadableSection(t *testing.T) {
	src := New(WithClientID(4))
	text := src.GetText("t")
	for i := range 6 {
		src.Transact(func(txn *Transaction) { text.Insert(txn, i, string(rune('a'+i)), nil) })
		src.Transact(func(txn *Transaction) { text.Insert(txn, 0, "x", nil) })
	}
	whole := EncodeStateAsUpdateV2(src, nil)

	unreadable := 0
	for cut := len(whole) - 1; cut > 0; cut-- {
		update := whole[:cut]
		dec, err := newV2Decoder(update)
		if err != nil {
			continue
		}
		start := *dec
		b := newPendingBudget(New(), StateVector{}, update, true)
		b.v2Start, b.v2Rest = &start, *dec.restDec
		_, walkErr := b.walkStructsV2()
		if walkErr == nil {
			continue
		}
		unreadable++
		b.remaining = 0
		err = b.checkBeforeApply(len(update))
		if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), scanReason(walkErr).Error()) {
			t.Fatalf("cut at %d: the check returned %v, want a refusal naming %v", cut, err, scanReason(walkErr))
		}
	}
	if unreadable == 0 {
		t.Fatal("no cut of the update made the V2 walk report an unreadable section")
	}
}
