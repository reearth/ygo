package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestYArrayPush_TombstonedTail_MatchesYjs is a #70 cross-impl regression.
//
// YArray.Push must anchor the new element after the last PHYSICAL item
// (tombstones included), matching Yjs's push (typeListPushGenerics). Before the
// fix, Push delegated to Insert(Len()), which anchors after the last LIVE item
// (leftNeighbourAt skips tombstones). So a push onto an array whose tail is a
// tombstone produced different YATA anchors than Yjs
// (origin=nil,rightOrigin=tombstone vs origin=tombstone,rightOrigin=nil), and
// two peers pushing concurrently ordered the results differently.
//
// Scenario: client3 pushes F; client1 receives F, deletes it, then pushes X (so
// X's right neighbour is the F tombstone); client2 independently pushes Y; merge
// all three. Real Yjs 13.6.30 yields ["Y","X"] (verified out-of-band via the
// cross-impl oracle and a hand-written Yjs script). ygo must match.
func TestYArrayPush_TombstonedTail_MatchesYjs(t *testing.T) {
	codecs := []struct {
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
	}

	build := func(deleteF bool, apply func(*Doc, []byte) error, encode func(*Doc) []byte) (*Doc, *Doc, *Doc) {
		// client3 pushes F.
		c3 := New(WithClientID(3))
		a3 := c3.GetArray("arr")
		c3.Transact(func(txn *Transaction) { a3.Push(txn, []any{"F"}) })

		// client1 receives F, optionally deletes it, then pushes X.
		c1 := New(WithClientID(1))
		require.NoError(t, apply(c1, encode(c3)))
		a1 := c1.GetArray("arr")
		if deleteF {
			c1.Transact(func(txn *Transaction) { a1.Delete(txn, 0, 1) })
		}
		c1.Transact(func(txn *Transaction) { a1.Push(txn, []any{"X"}) })

		// client2 independently pushes Y (never saw F).
		c2 := New(WithClientID(2))
		a2 := c2.GetArray("arr")
		c2.Transact(func(txn *Transaction) { a2.Push(txn, []any{"Y"}) })
		return c3, c1, c2
	}

	for _, tc := range codecs {
		t.Run(tc.name, func(t *testing.T) {
			// Bug scenario: F tombstoned. Yjs converges to ["Y","X"].
			c3, c1, c2 := build(true, tc.apply, tc.encode)
			merged := New(WithClientID(9))
			require.NoError(t, tc.apply(merged, tc.encode(c3)))
			require.NoError(t, tc.apply(merged, tc.encode(c1)))
			require.NoError(t, tc.apply(merged, tc.encode(c2)))
			got, err := merged.GetArray("arr").ToJSON()
			require.NoError(t, err)
			require.JSONEq(t, `["Y","X"]`, string(got), "push after a tombstoned tail must match Yjs ordering")

			// Control: F left live. Both ygo and Yjs converge to ["Y","F","X"];
			// this path already matched Yjs and must stay unchanged.
			c3b, c1b, c2b := build(false, tc.apply, tc.encode)
			mergedB := New(WithClientID(9))
			require.NoError(t, tc.apply(mergedB, tc.encode(c3b)))
			require.NoError(t, tc.apply(mergedB, tc.encode(c1b)))
			require.NoError(t, tc.apply(mergedB, tc.encode(c2b)))
			gotB, err := mergedB.GetArray("arr").ToJSON()
			require.NoError(t, err)
			require.JSONEq(t, `["Y","F","X"]`, string(gotB), "control (F live) must be unchanged")
		})
	}
}
