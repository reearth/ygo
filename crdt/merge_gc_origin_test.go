package crdt

import (
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
