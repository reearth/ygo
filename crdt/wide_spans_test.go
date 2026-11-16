package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

func wideSpanUpdate(clock, length uint64, deleted bool) []byte {
	e := encoding.NewEncoder()
	for _, n := range []uint64{1, 1, 5, clock} {
		e.WriteVarUint(n)
	}
	if deleted {
		e.WriteUint8(1)
		e.WriteUint8(1)
		e.WriteVarString("t3")
	} else {
		e.WriteUint8(0)
	}
	e.WriteVarUint(length)
	e.WriteVarUint(0)
	return e.Bytes()
}

func wideSpanOwnershipUpdates() [][]byte {
	const start = uint64(2) << 30
	const span = uint64(3) << 29
	e := encoding.NewEncoder()
	for _, n := range []uint64{1, 1, 9, 0} {
		e.WriteVarUint(n)
	}
	e.WriteUint8(0x84)
	e.WriteVarUint(5)
	e.WriteVarUint(start + span)
	e.WriteVarString("q")
	e.WriteVarUint(0)
	filler := encoding.NewEncoder()
	for _, n := range []uint64{1, 2, 5, 0} {
		filler.WriteVarUint(n)
	}
	for range 2 {
		filler.WriteUint8(0)
		filler.WriteVarUint(1 << 30)
	}
	filler.WriteVarUint(0)
	return [][]byte{
		wideSpanUpdate(start, span, false),
		wideSpanUpdate(start+span, span, false),
		wideSpanUpdate(start+span, span, true),
		filler.Bytes(),
		e.Bytes(),
	}
}

func TestUnit_WideSpans_CoalescingKeepsYjsOwner(t *testing.T) {
	d := New(WithClientID(99))
	for _, update := range wideSpanOwnershipUpdates() {
		require.NoError(t, ApplyUpdateV1(d, update, nil))
	}
	require.Equal(t, StateVector{5: 5368709120, 9: 1}, d.StateVector())
	require.Zero(t, d.PendingStats().Items)
	require.Empty(t, d.GetText("t3").ToString())
}
