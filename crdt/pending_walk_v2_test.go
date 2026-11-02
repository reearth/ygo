package crdt

import (
	"errors"
	"testing"
)

// The V2 first pass reports a struct section it cannot read, and the
// pending-cap check refuses such an update with the decoder's error, before
// the decoder reads it.
func TestUnit_PendingBudget_FirstPassV2ReportsAnUnreadableSection(t *testing.T) {
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
		// The group walk reads every struct, so it fails only where the cut is
		// in the struct section.
		if _, _, _, walkErr := supersededGroups(update, &start, *dec.restDec); walkErr == nil {
			continue
		}
		unreadable++
		b := newPendingBudget(New(), StateVector{}, update, true)
		b.v2Start, b.v2Rest = &start, *dec.restDec
		b.setSuperseded(nil) // as the apply does for one client
		b.remaining = 0
		plain := ApplyUpdateV2(New(), update, nil)
		err = b.checkBeforeApply(len(update))
		if plain == nil || !errors.Is(err, ErrInvalidUpdate) || err.Error() != plain.Error() {
			t.Fatalf("cut at %d: the check returned %v, the decoder %v", cut, err, plain)
		}
	}
	if unreadable == 0 {
		t.Fatal("no cut of the update left its struct section unreadable")
	}
}
