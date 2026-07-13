package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestYText_InsertAdjacentTombstone_MatchesYjs is a #70 cross-impl regression
// (the text-path mirror of the array Push fix #158).
//
// YText.Insert anchored a new run relative to the last LIVE item, leaving
// originRight pointing AT an adjacent tombstone — so the run landed BEFORE the
// tombstone. Yjs's text insert advances its cursor PAST deleted items before
// anchoring (minimizeAttributeChanges), so the run lands AFTER them
// (origin=tombstone, rightOrigin=next-live). When two peers concurrently insert
// next to the same tombstone the two anchorings order the runs differently.
//
// Scenario (verified out-of-band against Yjs 13.6.30): peer0 (client1) inserts
// "語"; peer3 (client4) receives it, deletes it (tombstone), inserts "fcd" at 0;
// peer0 concurrently inserts "bba" after "語"; merge. Both anchor origin="語",
// so it is a same-origin conflict resolved by clientID (1<4): "bbafcd". Before
// the fix ygo produced "fcdbba".
func TestYText_InsertAdjacentTombstone_MatchesYjs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encode func(*Doc) []byte
		apply  func(*Doc, []byte) error
	}{
		{"V1",
			func(d *Doc) []byte { return EncodeStateAsUpdateV1(d, nil) },
			func(d *Doc, u []byte) error { return ApplyUpdateV1(d, u, nil) }},
		{"V2",
			func(d *Doc) []byte { return EncodeStateAsUpdateV2(d, nil) },
			func(d *Doc, u []byte) error { return ApplyUpdateV2(d, u, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// peer0 inserts "語".
			c0 := New(WithClientID(1))
			t0 := c0.GetText("t")
			c0.Transact(func(txn *Transaction) { t0.Insert(txn, 0, "語", nil) })

			// peer3 receives it, deletes it (tombstone), inserts "fcd" at 0.
			c3 := New(WithClientID(4))
			require.NoError(t, tc.apply(c3, tc.encode(c0)))
			t3 := c3.GetText("t")
			c3.Transact(func(txn *Transaction) { t3.Delete(txn, 0, 1) })
			c3.Transact(func(txn *Transaction) { t3.Insert(txn, 0, "fcd", nil) })

			// peer0 concurrently inserts "bba" after "語".
			c0.Transact(func(txn *Transaction) { t0.Insert(txn, 1, "bba", nil) })

			// Merge both onto a fresh peer.
			m := New(WithClientID(9))
			require.NoError(t, tc.apply(m, tc.encode(c0)))
			require.NoError(t, tc.apply(m, tc.encode(c3)))
			got, err := m.GetText("t").ToJSON()
			require.NoError(t, err)
			require.JSONEq(t, `"bbafcd"`, string(got),
				"text run inserted next to a tombstone must match Yjs ordering")
		})
	}
}
