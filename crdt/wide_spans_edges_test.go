package crdt

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

func TestUnit_WideSpans_RoundTripConvertDiffAndReplay(t *testing.T) {
	for _, length := range []uint64{1<<31 - 1, 1 << 31, 1<<32 + 1, 1<<53 - 1} {
		for _, deleted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/deleted=%v", length, deleted), func(t *testing.T) {
				v1 := wideSpanUpdate(0, length, deleted)
				ds := encoding.NewEncoder()
				for _, n := range []uint64{1, 5, 1, 0, length} {
					ds.WriteVarUint(n)
				}
				v1 = append(v1[:len(v1)-1], ds.Bytes()...)
				v2, err := UpdateV1ToV2(v1)
				require.NoError(t, err)
				back, err := UpdateV2ToV1(v2)
				require.NoError(t, err)
				require.Equal(t, v1, back)
				for _, version := range []int{1, 2} {
					update := v1
					apply := ApplyUpdateV1
					encode := EncodeStateAsUpdateV1
					diff := DiffUpdateV1
					if version == 2 {
						update = v2
						apply = ApplyUpdateV2
						encode = EncodeStateAsUpdateV2
						diff = DiffUpdateV2
					}
					doc := New(WithClientID(99), WithGC(false))
					require.NoError(t, apply(doc, update, nil))
					require.Equal(t, StateVector{5: length}, doc.StateVector())
					require.Empty(t, doc.GetText("t3").ToString())
					require.Zero(t, doc.GetText("t3").Len())
					clone := New(WithClientID(98), WithGC(false))
					require.NoError(t, apply(clone, encode(doc, nil), nil))
					require.Equal(t, doc.StateVector(), clone.StateVector())
					cut := length - 1
					suffix, err := diff(update, StateVector{5: cut})
					require.NoError(t, err)
					require.Equal(t, encode(doc, StateVector{5: cut}), suffix)
					parked := New(WithClientID(97), WithGC(false))
					require.NoError(t, apply(parked, suffix, nil))
					require.Equal(t, 1, parked.PendingStats().Items)
					relay := New(WithClientID(96), WithGC(false))
					require.NoError(t, apply(relay, encode(parked, nil), nil))
					require.Equal(t, 1, relay.PendingStats().Items)
					prefix := wideSpanUpdate(0, cut, deleted)
					if version == 2 {
						prefix, err = UpdateV1ToV2(prefix)
						require.NoError(t, err)
					}
					require.NoError(t, apply(relay, prefix, nil))
					require.Zero(t, relay.PendingStats().Items)
					require.Equal(t, doc.StateVector(), relay.StateVector())
					require.Empty(t, relay.GetText("t3").ToString())
				}
			})
		}
	}
}

func TestUnit_WideSpans_InteriorOriginsAndDeleteBoundary(t *testing.T) {
	const length = uint64(5) << 30
	const cut = uint64(3) << 30
	for _, right := range []bool{false, true} {
		for _, v2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("right=%v/v2=%v", right, v2), func(t *testing.T) {
				doc := New(WithClientID(99), WithGC(false))
				apply := func(update []byte) {
					if v2 {
						var err error
						update, err = UpdateV1ToV2(update)
						require.NoError(t, err)
						require.NoError(t, ApplyUpdateV2(doc, update, nil))
					} else {
						require.NoError(t, ApplyUpdateV1(doc, update, nil))
					}
				}
				apply(wideSpanUpdate(0, length, true))
				e := encoding.NewEncoder()
				for _, n := range []uint64{1, 1, 9, 0} {
					e.WriteVarUint(n)
				}
				if right {
					e.WriteUint8(0x44)
				} else {
					e.WriteUint8(0x84)
				}
				e.WriteVarUint(5)
				e.WriteVarUint(cut)
				e.WriteVarString("q")
				e.WriteVarUint(0)
				apply(e.Bytes())
				require.Equal(t, "q", doc.GetText("t3").ToString())
				require.Equal(t, StateVector{5: length, 9: 1}, doc.StateVector())
				e = encoding.NewEncoder()
				for _, n := range []uint64{0, 1, 5, 1, cut, 1} {
					e.WriteVarUint(n)
				}
				apply(e.Bytes())
				require.Equal(t, "q", doc.GetText("t3").ToString())
				doc.Transact(func(txn *Transaction) {
					item := doc.store.getItemCleanStart(txn, ID{Client: 5, Clock: cut})
					require.NotNil(t, item)
					require.Equal(t, cut, item.ID.Clock)
					require.Equal(t, length-cut, item.Content.Len())
					left := doc.store.getItemCleanEnd(txn, 5, cut-1)
					require.NotNil(t, left)
					require.Equal(t, cut, left.Content.Len())
				})
			})
		}
	}
}

func TestUnit_WideSpans_SkipAndMoveRetainTheirLengths(t *testing.T) {
	const length = uint64(1)<<32 + 1
	for _, tag := range []byte{10, 11} {
		e := encoding.NewEncoder()
		count := uint64(1)
		if tag == 10 {
			count = 2
		}
		for _, n := range []uint64{1, count, 5, 0} {
			e.WriteVarUint(n)
		}
		e.WriteUint8(tag)
		if tag == 11 {
			e.WriteUint8(1)
			e.WriteVarString("t3")
			e.WriteVarUint(9)
			e.WriteVarUint(0)
		}
		e.WriteVarUint(length)
		if tag == 10 {
			e.WriteUint8(4)
			e.WriteUint8(1)
			e.WriteVarString("t3")
			e.WriteVarString("q")
		}
		e.WriteVarUint(0)
		v1 := e.Bytes()
		v2, err := UpdateV1ToV2(v1)
		require.NoError(t, err)
		back, err := UpdateV2ToV1(v2)
		require.NoError(t, err)
		items, _, err := decodeStructsV1(New(), back)
		require.NoError(t, err)
		if tag == 11 {
			require.Equal(t, v1, back)
			require.Equal(t, length, items[5][0].Content.(*ContentMove).TargetLen)
		} else {
			require.Equal(t, length, items[5][0].ID.Clock)
			require.Equal(t, "q", items[5][0].Content.(*ContentString).Str)
			for _, version := range []int{1, 2} {
				doc := New(WithClientID(99))
				update := back
				prefix := wideSpanUpdate(0, length, false)
				apply := ApplyUpdateV1
				if version == 2 {
					apply = ApplyUpdateV2
					update = v2
					prefix, err = UpdateV1ToV2(prefix)
					require.NoError(t, err)
				}
				require.NoError(t, apply(doc, update, nil))
				require.Equal(t, 1, doc.PendingStats().Items)
				require.NoError(t, apply(doc, prefix, nil))
				require.Equal(t, "q", doc.GetText("t3").ToString())
				require.Equal(t, StateVector{5: length + 1}, doc.StateVector())
			}
		}
	}
}

type wideCountableContent struct{ Content }

func (wideCountableContent) Len() uint64 { return uint64(1) << 63 }

func TestUnit_WideSpans_NativePositionRefusalPrecedesMutation(t *testing.T) {
	doc := New(WithClientID(99))
	text := doc.GetText("t3")
	item := &Item{ID: ID{Client: 5}, Parent: &text.abstractType, Content: wideCountableContent{NewContentString("q")}}
	require.Panics(t, func() { doc.Transact(func(txn *Transaction) { item.integrate(txn, 0) }) })
	require.Empty(t, doc.StateVector())
	require.Nil(t, text.start)
	require.Zero(t, text.Len())
}

func TestUnit_WideSpans_V2KeyClockDoesNotTruncateToNativeIndex(t *testing.T) {
	e := newV2Encoder()
	e.keyClockEnc.Write(1 << 32)
	e.writeString("new")
	d, err := newV2Decoder(e.toBytes())
	require.NoError(t, err)
	d.keys = []string{"existing"}
	key, err := d.readKey()
	require.NoError(t, err)
	require.Equal(t, "new", key)
	require.Equal(t, []string{"existing", "new"}, d.keys)
}

func TestUnit_WideSpans_MaterializedSpliceRefusesBeforeMutation(t *testing.T) {
	for _, content := range []Content{NewContentString("q"), NewContentAny([]any{1}), NewContentJSON([]any{1})} {
		before := content.Copy()
		require.Panics(t, func() { content.Splice(1<<32 + 1) })
		require.Equal(t, before, content)
	}
}
