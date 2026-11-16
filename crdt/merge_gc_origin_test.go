package crdt

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// A merged or diffed update keeps an origin that names a GC struct, as Yjs's
// mergeUpdates and diffUpdate do, so it applies as the updates it came from.
func TestUnit_MergeUpdates_KeepsOriginsThatNameAGCStruct(t *testing.T) {
	helloV1, heZZllo := gcNeighbourText(t)
	for _, f := range gcNeighbourFormats {
		for _, tc := range gcNeighbourPositions {
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				var inputs [][]byte
				for _, v1 := range [][]byte{helloV1, gcOverZZ, peerInsert(t, 7, heZZllo, tc.pos, "b")} {
					u, err := f.convert(v1)
					require.NoError(t, err)
					inputs = append(inputs, u)
				}
				merged, err := f.merge(inputs...)
				require.NoError(t, err)
				room := New(WithClientID(1))
				require.NoError(t, f.apply(room, merged, nil))
				requireGCStructs(t, f, room, textOf, "hello", ID{Client: 7, Clock: 0})

				diff, err := f.diff(merged, StateVector{5: 5})
				require.NoError(t, err)
				room = New(WithClientID(1))
				require.NoError(t, f.apply(room, inputs[0], nil))
				require.NoError(t, f.apply(room, diff, nil))
				requireGCStructs(t, f, room, textOf, "hello", ID{Client: 7, Clock: 0})
			})
		}
	}
}

// A document's encoded state keeps the GC origin of a parked item, so a replica
// loaded from it stores the item as a GC struct once its right origin arrives.
func TestUnit_EncodeState_KeepsTheGCOriginOfAParkedItem(t *testing.T) {
	hello, heZZllo := gcNeighbourText(t)
	q := peerInsert(t, 9, hello, 2, "Q") // "heQllo", without "ZZ"
	state, err := MergeUpdatesV1(heZZllo, q)
	require.NoError(t, err)
	b := peerInsert(t, 7, state, 4, "b") // "heZZbQllo": origin in "ZZ", right origin the live "Q"
	for _, f := range gcNeighbourFormats {
		t.Run(f.name, func(t *testing.T) {
			room := New(WithClientID(1))
			for _, u := range [][]byte{hello, gcOverZZ, b} {
				f.applyV1(t, room, u)
			}
			require.Equal(t, 1, room.PendingStats().Items)
			fromParked := New(WithClientID(3))
			require.NoError(t, f.apply(fromParked, f.encode(room, nil), nil))
			for _, doc := range []*Doc{room, fromParked} {
				f.applyV1(t, doc, q)
				requireGCStructs(t, f, doc, textOf, "heQllo", ID{Client: 7, Clock: 0})
			}
		})
	}
}

// A room whose parked GC struct cuts a parked item writes the tail's origin, a
// clock in that GC struct, as Yjs 13.6.32 does, so a replica of its state shows
// the room's text. It checks the replica, not that its state equals the room's:
// the room keeps the tail as an item until its pending queue merges parked
// structs as Yjs does.
func TestUnit_EncodeState_KeepsTheGCOriginOfACutParkedItem(t *testing.T) {
	item := []byte{1, 1, 2, 2, 0x84, 99, 0, 2, 'a', 'b', 0} // client 2's "ab" at clock 2, origin 99@0
	gc := []byte{1, 1, 2, 1, 0, 2, 0}                       // a GC struct over client 2's clocks 1-2
	filler := []byte{1, 1, 2, 0, 0, 1, 0}                   // a GC struct over client 2's clock 0
	yjs := map[string]string{                               // Yjs 13.6.32's encoded state of the room
		"V1": "010202010002840202016200",
		"V2": "000002420001040003000084030162010000010201020100",
	}
	show := func(doc *Doc) string { return doc.GetText("").ToString() }
	for _, f := range gcNeighbourFormats {
		t.Run(f.name, func(t *testing.T) {
			room := New(WithClientID(1))
			for _, u := range [][]byte{item, gc} {
				f.applyV1(t, room, u)
			}
			state := f.encode(room, nil)
			require.Equal(t, yjs[f.name], hex.EncodeToString(state))
			replica := New(WithClientID(3))
			require.NoError(t, f.apply(replica, state, nil))
			for _, doc := range []*Doc{room, replica} {
				f.applyV1(t, doc, filler)
			}
			require.Empty(t, show(room))
			require.Equal(t, show(room), show(replica))
			require.True(t, findStruct(t, replica, ID{Client: 2, Clock: 3}).isGCOrphan(), "the replica's 2@3 must be a GC struct")
		})
	}
}
