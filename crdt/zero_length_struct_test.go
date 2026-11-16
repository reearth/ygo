package crdt

import (
	"encoding/hex"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// zeroLengthGroup is a V1 update of one group of client's structs from clock,
// each a [kind, length] pair: s a string item, d a deleted-content item, a an
// any item, g a GC struct and k a Skip, all items root children of "t".
func zeroLengthGroup(client ClientID, clock uint64, structs [][2]any) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(uint64(len(structs)))
	enc.WriteVarUint(uint64(client))
	enc.WriteVarUint(clock)
	for _, st := range structs {
		kind, n := st[0].(string), st[1].(int)
		switch kind {
		case "g":
			enc.WriteUint8(0)
			enc.WriteVarUint(uint64(n))
			continue
		case "k":
			enc.WriteUint8(10)
			enc.WriteVarUint(uint64(n))
			continue
		case "d":
			enc.WriteUint8(1)
		case "a":
			enc.WriteUint8(8)
		case "s":
			enc.WriteUint8(wireString)
		}
		enc.WriteVarUint(1)
		enc.WriteVarString("t")
		switch kind {
		case "d":
			enc.WriteVarUint(uint64(n))
		case "a":
			enc.WriteVarUint(uint64(n))
			for range n {
				enc.WriteAny("v")
			}
		case "s":
			text := make([]byte, n)
			for i := range text {
				text[i] = 'x'
			}
			enc.WriteVarString(string(text))
		}
	}
	enc.WriteVarUint(0)
	return enc.Bytes()
}

// yjsV2 is each case's update as Yjs 13.6.32 convertUpdateFormatV1ToV2 writes
// it, zero-length structs kept.
var yjsV2 = map[string]string{
	"s/alone at its client's clock": "000001030000010404017401000101000001010400",
	"s/alone past a gap":            "000001030000010404017401000101000001010600",
	"s/after a struct":              "0000010300000104090474787874010201000101000001020400",
	"s/before a struct":             "0000010300000104090474747878010001020101000001020400",
	"d/alone at its client's clock": "000001030000010103017401010100010001010400",
	"d/alone past a gap":            "000001030000010103017401010100010001010600",
	"d/after a struct":              "00000103000003040001080474787874010201010100010001020400",
	"d/before a struct":             "00000103000003010004080474747878410002010100010001020400",
	"a/alone at its client's clock": "000001030000010803017401010100010001010400",
	"a/alone past a gap":            "000001030000010803017401010100010001010600",
	"a/after a struct":              "00000103000003040008080474787874010201010100010001020400",
	"a/before a struct":             "00000103000003080004080474747878410002010100010001020400",
	"g/alone at its client's clock": "000001030000010001000000010001010400",
	"g/alone past a gap":            "000001030000010001000000010001010600",
	"g/after a struct":              "0000010300000304000006037478780102010100010001020400",
	"g/before a struct":             "0000010300000300000406037478780102010100010001020400",
}

// An item or GC struct that spans no clock is refused before any change, in
// V1 and V2, at its client's clock or past a gap, alone or beside another
// struct, on an empty queue or behind one, with nothing integrated, parked,
// deleted or told to an observer; a zero-length Skip is not refused.
func TestUnit_ApplyUpdate_RefusesAZeroLengthStructBeforeAnyChange(t *testing.T) {
	// Client 50's "z" at clock 1, which parks behind a gap.
	parksOne := zeroLengthGroup(50, 1, [][2]any{{"s", 1}})
	seed := zeroLengthGroup(3, 0, [][2]any{{"s", 4}})
	for _, kind := range []string{"s", "d", "a", "g"} {
		for _, at := range []struct {
			name    string
			clock   uint64
			structs [][2]any
		}{
			{"alone at its client's clock", 4, [][2]any{{kind, 0}}},
			{"alone past a gap", 6, [][2]any{{kind, 0}}},
			{"after a struct", 4, [][2]any{{"s", 2}, {kind, 0}}},
			{"before a struct", 4, [][2]any{{kind, 0}, {"s", 2}}},
		} {
			for _, queued := range []bool{false, true} {
				update := zeroLengthGroup(3, at.clock, at.structs)
				v2, err := hex.DecodeString(yjsV2[kind+"/"+at.name])
				require.NoError(t, err)
				for _, f := range []struct {
					name   string
					update []byte
					apply  func(*Doc, []byte, any) error
				}{{"V1", update, ApplyUpdateV1}, {"V2", v2, ApplyUpdateV2}} {
					t.Run(fmt.Sprintf("%s/%s/%s/queued=%v", f.name, kind, at.name, queued), func(t *testing.T) {
						doc := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
						require.NoError(t, ApplyUpdateV1(doc, seed, nil))
						if queued {
							require.NoError(t, ApplyUpdateV1(doc, parksOne, nil))
						}
						before := EncodeStateAsUpdateV1(doc, nil)
						parked := doc.PendingStats()
						events := 0
						doc.OnUpdate(func([]byte, any) { events++ })
						err := f.apply(doc, f.update, nil)
						require.ErrorIs(t, err, ErrInvalidUpdate)
						require.ErrorContains(t, err, errZeroLengthStruct.Error())
						assert.Equal(t, before, EncodeStateAsUpdateV1(doc, nil), "the document is unchanged")
						assert.Equal(t, parked, doc.PendingStats(), "the queue is unchanged")
						assert.Zero(t, events, "no observer is told")
					})
				}
			}
		}
	}

	doc := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(doc, zeroLengthGroup(3, 0, [][2]any{{"k", 0}, {"s", 1}}), nil))
	assert.Equal(t, "x", doc.GetText("t").ToString(), "a zero-length Skip is read past")
}

// A zero-length struct beside a deletion of its client's items refuses the
// whole update before any change, so the deletion does not apply and nothing
// is relayed.
func TestUnit_ApplyUpdateV1Landed_RefusesAZeroLengthStructBesideADeletion(t *testing.T) {
	seed := zeroLengthGroup(3, 0, [][2]any{{"s", 4}})
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteVarUint(3)
	enc.WriteVarUint(6)
	enc.WriteUint8(wireString)
	enc.WriteVarUint(1)
	enc.WriteVarString("t")
	enc.WriteVarString("")
	enc.WriteVarUint(1)
	enc.WriteVarUint(3)
	enc.WriteVarUint(1)
	enc.WriteVarUint(0)
	enc.WriteVarUint(4)

	room := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(room, seed, nil))
	landed, err := ApplyUpdateV1Landed(room, enc.Bytes(), nil)
	require.ErrorIs(t, err, ErrInvalidUpdate)
	require.ErrorContains(t, err, errZeroLengthStruct.Error())
	require.Nil(t, landed, "nothing to relay")
	assert.Equal(t, "xxxx", room.GetText("t").ToString(), "the deletion did not apply")
}

// A zero-length map item, which Yjs would make the key's value and lose the
// key's content with, is refused before any change, so a peer given only what
// the room accepted keeps the key, as the room does.
func TestUnit_ApplyUpdate_RefusesAZeroLengthMapItem(t *testing.T) {
	setOld := encoding.NewEncoder()
	setOld.WriteVarUint(1)
	setOld.WriteVarUint(1)
	setOld.WriteVarUint(1)
	setOld.WriteVarUint(0)
	setOld.WriteUint8(0x28) // ContentAny with a parent key
	setOld.WriteVarUint(1)
	setOld.WriteVarString("m")
	setOld.WriteVarString("k")
	setOld.WriteVarUint(1)
	setOld.WriteAny("old")
	setOld.WriteVarUint(0)

	// Client 3's empty string for m["k"] at clock 1, past a gap at clock 0.
	empty := encoding.NewEncoder()
	empty.WriteVarUint(1)
	empty.WriteVarUint(1)
	empty.WriteVarUint(3)
	empty.WriteVarUint(1)
	empty.WriteUint8(0x24) // ContentString with a parent key
	empty.WriteVarUint(1)
	empty.WriteVarString("m")
	empty.WriteVarString("k")
	empty.WriteVarString("")
	empty.WriteVarUint(0)
	fill := zeroLengthGroup(3, 0, [][2]any{{"s", 1}})

	room := New(WithClientID(9))
	peer := New(WithClientID(8))
	for _, d := range []*Doc{room, peer} {
		require.NoError(t, ApplyUpdateV1(d, setOld.Bytes(), nil))
	}
	landed, err := ApplyUpdateV1Landed(room, empty.Bytes(), nil)
	require.ErrorIs(t, err, ErrInvalidUpdate)
	require.ErrorContains(t, err, errZeroLengthStruct.Error())
	require.Nil(t, landed, "nothing to relay")
	for _, d := range []*Doc{room, peer} {
		require.NoError(t, ApplyUpdateV1(d, fill, nil))
		v, ok := d.GetMap("m").Get("k")
		require.True(t, ok)
		require.Equal(t, "old", v)
	}
	assert.Equal(t, EncodeStateAsUpdateV1(room, nil), EncodeStateAsUpdateV1(peer, nil))
}

// State with a struct of length zero in it, which ygo 1.55.0 and earlier
// could store and the apply refuses, applies after a merge or a conversion,
// which drop that struct and keep the rest. V2 is Yjs 13.6.32
// convertUpdateFormatV1ToV2 of V1, the struct kept.
func TestUnit_MergeUpdates_DropAZeroLengthStructSoStoredStateApplies(t *testing.T) {
	for _, tc := range []struct {
		name, v1, v2 string
		check        func(*testing.T, *Doc)
	}{
		{
			"text, and an empty string parked past a gap",
			"020101000401017403616263010306040101740000",
			"0000020103000001040a0574616263740103010001010000020100010600",
			func(t *testing.T, d *Doc) { assert.Equal(t, "abc", d.GetText("t").ToString()) },
		},
		{
			"an empty map item parked past a gap",
			"010103042401016d016b0000",
			"000001030000012406026d6b4100000101000001010400",
			func(t *testing.T, d *Doc) {
				_, ok := d.GetMap("m").Get("k")
				assert.False(t, ok)
			},
		},
		{
			"YArray.Insert of no values after two values",
			"0102070008010161027701787701798807010000",
			"0000024700010200030800880301610101010002020001020077017877017900",
			func(t *testing.T, d *Doc) { assert.Equal(t, []any{"x", "y"}, d.GetArray("a").ToSlice()) },
		},
		{
			"YArray.Push of no values",
			"0102070008010161017701788807000000",
			"0000024700010000030800880301610101010002010001020077017800",
			func(t *testing.T, d *Doc) { assert.Equal(t, []any{"x"}, d.GetArray("a").ToSlice()) },
		},
		{
			"YArray.Insert of no values after text",
			"0102070004010174026162080101610000",
			"00000107000003040008080474616261010201010100010001020000",
			func(t *testing.T, d *Doc) {
				assert.Equal(t, "ab", d.GetText("t").ToString())
				assert.Empty(t, d.GetArray("a").ToSlice())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v1, err := hex.DecodeString(tc.v1)
			require.NoError(t, err)
			v2, err := hex.DecodeString(tc.v2)
			require.NoError(t, err)
			require.ErrorContains(t, ApplyUpdateV1(New(), v1, nil), errZeroLengthStruct.Error())
			require.ErrorContains(t, ApplyUpdateV2(New(), v2, nil), errZeroLengthStruct.Error())

			for _, heal := range []struct {
				name    string
				run     func() ([]byte, error)
				version int
			}{
				{"MergeUpdatesV1", func() ([]byte, error) { return MergeUpdatesV1(v1) }, 1},
				{"UpdateV1ToV2", func() ([]byte, error) { return UpdateV1ToV2(v1) }, 2},
				{"MergeUpdatesV2", func() ([]byte, error) { return MergeUpdatesV2(v2) }, 2},
				{"UpdateV2ToV1", func() ([]byte, error) { return UpdateV2ToV1(v2) }, 1},
			} {
				healed, err := heal.run()
				require.NoError(t, err, heal.name)
				d := New()
				require.NoError(t, applyVersion(d, heal.version, healed), heal.name)
				tc.check(t, d)
				assert.Zero(t, d.PendingStats().Items, heal.name)
			}
		})
	}
}
