package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var gcOriginFormats = []struct {
	name   string
	v2     bool
	apply  func(*Doc, []byte, any) error
	encode func(*Doc, StateVector) []byte
	decode func(*Doc, []byte) (map[ClientID][]*Item, DeleteSet, error)
	merge  func(...[]byte) ([]byte, error)
}{
	{"V1", false, ApplyUpdateV1, EncodeStateAsUpdateV1, decodeStructsV1, MergeUpdatesV1},
	{"V2", true, ApplyUpdateV2, EncodeStateAsUpdateV2, decodeStructsV2, MergeUpdatesV2},
}

// gcAndHi returns a GC struct over client 2's clocks 5..6 and "hi" at 7..8,
// whose origin 2:6 is inside the GC struct.
func gcAndHi() []*Item {
	return []*Item{
		{ID: ID{Client: 2, Clock: 5}, Content: NewContentDeleted(2), Deleted: true},
		{ID: ID{Client: 2, Clock: 7}, Origin: &ID{Client: 2, Clock: 6}, Content: NewContentString("hi")},
	}
}

// clientTwoUpdate encodes items of client 2 as one update.
func clientTwoUpdate(v2 bool, items ...*Item) []byte {
	perClient := map[ClientID][]*Item{2: items}
	if v2 {
		return encodeStructStoreV2(perClient, newDeleteSet(), nil)
	}
	return encodeStructStoreV1(perClient, newDeleteSet(), nil)
}

// applyClientTwoFiller applies client 2's missing clocks 0..4, as a GC struct.
func applyClientTwoFiller(t *testing.T, v2 bool, docs ...*Doc) {
	t.Helper()
	filler := clientTwoUpdate(v2, &Item{ID: ID{Client: 2, Clock: 0}, Content: NewContentDeleted(5), Deleted: true})
	for _, d := range docs {
		if v2 {
			require.NoError(t, ApplyUpdateV2(d, filler, nil))
		} else {
			require.NoError(t, ApplyUpdateV1(d, filler, nil))
		}
	}
}

func requireSameDoc(t *testing.T, want, got *Doc) {
	t.Helper()
	require.Equal(t, want.GetText("").ToString(), got.GetText("").ToString(), `the text of the root type ""`)
	require.Equal(t, want.StateVector(), got.StateVector())
	require.Equal(t, EncodeStateAsUpdateV1(want, nil), EncodeStateAsUpdateV1(got, nil))
}

// A document's encoded state keeps the GC origin of a parked item, so a
// replica loaded from it turns the item into a GC struct, as the document
// does, when the missing clocks arrive.
func TestUnit_EncodeState_KeepsTheGCOriginOfAParkedItem(t *testing.T) {
	for _, f := range gcOriginFormats {
		t.Run(f.name, func(t *testing.T) {
			room := New(WithClientID(1))
			require.NoError(t, f.apply(room, clientTwoUpdate(f.v2, gcAndHi()...), nil))
			require.Equal(t, 2, room.PendingStats().Items)

			state := f.encode(room, nil)
			structs, _, err := f.decode(New(), state)
			require.NoError(t, err)
			var hi *Item
			for _, it := range structs[2] {
				if it.ID.Clock == 7 {
					hi = it
				}
			}
			require.NotNil(t, hi)
			assert.Equal(t, &ID{Client: 2, Clock: 6}, hi.Origin, `the encoded state keeps the origin of "hi"`)

			replica := New(WithClientID(9))
			require.NoError(t, f.apply(replica, state, nil))
			applyClientTwoFiller(t, f.v2, room, replica)
			requireSameDoc(t, room, replica)
		})
	}
}

// MergeUpdatesV1 and MergeUpdatesV2 keep an origin that names a GC struct, so
// the merged update is the update the inputs make up.
func TestUnit_MergeUpdatesV1_KeepsAnOriginThatNamesAGCStruct(t *testing.T) {
	testMergeKeepsAGCOrigin(t, 0)
}

func TestUnit_MergeUpdatesV2_KeepsAnOriginThatNamesAGCStruct(t *testing.T) {
	testMergeKeepsAGCOrigin(t, 1)
}

func testMergeKeepsAGCOrigin(t *testing.T, format int) {
	f := gcOriginFormats[format]
	items := gcAndHi()
	whole := clientTwoUpdate(f.v2, items...)
	for _, tc := range []struct {
		name   string
		inputs [][]byte
	}{
		{"one update", [][]byte{whole}},
		{"one update per struct", [][]byte{clientTwoUpdate(f.v2, items[0]), clientTwoUpdate(f.v2, items[1])}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := f.merge(tc.inputs...)
			require.NoError(t, err)
			assert.Equal(t, whole, merged, "the merged update is the update the inputs make up")

			fromInputs, fromMerged := New(WithClientID(1)), New(WithClientID(9))
			for _, u := range tc.inputs {
				require.NoError(t, f.apply(fromInputs, u, nil))
			}
			require.NoError(t, f.apply(fromMerged, merged, nil))
			applyClientTwoFiller(t, f.v2, fromInputs, fromMerged)
			requireSameDoc(t, fromInputs, fromMerged)
		})
	}
}

// The format converters keep an origin that names a GC struct.
func TestUnit_UpdateFormatConverters_KeepAnOriginThatNamesAGCStruct(t *testing.T) {
	v1, v2 := clientTwoUpdate(false, gcAndHi()...), clientTwoUpdate(true, gcAndHi()...)
	got, err := UpdateV1ToV2(v1)
	require.NoError(t, err)
	require.Equal(t, v2, got)
	got, err = UpdateV2ToV1(v2)
	require.NoError(t, err)
	require.Equal(t, v1, got)
}
