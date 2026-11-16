package crdt

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// landedParking returns an update in which client sets n keys on a map that
// another client created and the receiver never got, so every entry parks.
func landedParking(t *testing.T, client uint64, n int) []byte {
	t.Helper()
	author := newTestDoc(client + 1000)
	am := author.GetMap("m")
	author.Transact(func(txn *Transaction) { am.Set(txn, "nested", NewMapPrelim()) })
	editor := newTestDoc(client)
	require.NoError(t, ApplyUpdateV1(editor, EncodeStateAsUpdateV1(author, nil), nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested := v.(*YMap)
	editor.Transact(func(txn *Transaction) {
		for i := range n {
			nested.Set(txn, "k"+strconv.Itoa(i), i)
		}
	})
	return EncodeStateAsUpdateV1(editor, author.StateVector())
}

func TestUnit_ApplyUpdateV1Landed(t *testing.T) {
	// The room holds 50 deletions and 4 parked entries, which an encoding of
	// its state would carry, and the writer's key w.a (client 3, clock 100).
	writer := newTestDoc(3)
	wt := writer.GetText("t")
	wm := writer.GetMap("w")
	writer.Transact(func(txn *Transaction) { wt.Insert(txn, 0, strings.Repeat("x", 100), nil) })
	writer.Transact(func(txn *Transaction) { wt.Delete(txn, 0, 50) })
	writer.Transact(func(txn *Transaction) { wm.Set(txn, "a", 1) })
	written := EncodeStateAsUpdateV1(writer, nil)
	newRoom := func() *Doc {
		// The cap leaves room for this update's delete ranges, so the
		// pending-cap checks that run before the apply (checkBeforeApply,
		// checkParkedDeletes) do not read its delete set: the apply reaches
		// it and fails there, after its structs integrated or parked.
		room := New(WithClientID(100), WithMaxPendingItems(100))
		require.NoError(t, ApplyUpdateV1(room, written, nil))
		require.NoError(t, ApplyUpdateV1(room, landedParking(t, 2, 4), nil))
		require.Equal(t, 4, room.PendingStats().Items)
		return room
	}

	// The editor's text and overwrite integrate and five map entries park
	// before its cut-short delete set fails the apply; the range read before
	// the cut, which deletes one x, applies.
	editor := newTestDoc(7)
	require.NoError(t, ApplyUpdateV1(editor, written, nil))
	author := newTestDoc(1)
	am := author.GetMap("m")
	author.Transact(func(txn *Transaction) { am.Set(txn, "nested", NewMapPrelim()) })
	require.NoError(t, ApplyUpdateV1(editor, EncodeStateAsUpdateV1(author, nil), nil))
	v, _ := editor.GetMap("m").Get("nested")
	nested := v.(*YMap)
	et, em := editor.GetText("t"), editor.GetMap("w")
	before := editor.StateVector() // includes the author's map, so it is not sent
	editor.Transact(func(txn *Transaction) {
		et.Insert(txn, 0, "abc", nil)
		et.Delete(txn, 3, 1)
		em.Set(txn, "a", 2)
		for i := range 5 {
			nested.Set(txn, "e"+strconv.Itoa(i), i)
		}
	})
	cut := EncodeStateAsUpdateV1(editor, before)
	cut = cut[:len(cut)-1]

	room := newRoom()
	landed, err := ApplyUpdateV1Landed(room, cut, nil)
	require.ErrorIs(t, err, ErrInvalidUpdate)
	require.Equal(t, "abc"+strings.Repeat("x", 49), room.GetText("t").ToString(), "the insertion and the deletion read before the cut applied")
	require.NotNil(t, landed)

	ids, err := ContentIDsFromUpdateV1(landed)
	require.NoError(t, err)
	assert.Equal(t, []ClientID{7}, ids.Inserts.Clients(), "only structs this apply integrated or parked")
	assert.Equal(t, []IDRange{{Clock: 0, Len: 9}}, ids.Inserts.Ranges(7), `"abc", w.a and the five parked entries`)
	assert.Equal(t, []ClientID{3}, ids.Deletes.Clients(), "only the deletions this apply made")
	assert.Equal(t, []IDRange{{Clock: 50, Len: 1}, {Clock: 100, Len: 1}}, ids.Deletes.Ranges(3), "the deleted x and the overwritten w.a")

	// A peer that held what the room held before ends where the room did,
	// parked entries included.
	peer := newRoom()
	require.NoError(t, ApplyUpdateV1(peer, landed, nil))
	assert.Equal(t, room.GetText("t").ToString(), peer.GetText("t").ToString())
	assert.Equal(t, EncodeStateAsUpdateV1(room, nil), EncodeStateAsUpdateV1(peer, nil))
	assert.Equal(t, room.PendingStats(), peer.PendingStats())

	t.Run("an update that does not decode, and one that applies, land nil", func(t *testing.T) {
		landed, err := ApplyUpdateV1Landed(newRoom(), EncodeStateAsUpdateV1(editor, nil)[:0], nil)
		require.Error(t, err, "an empty byte slice does not decode")
		assert.Nil(t, landed, "nothing integrated")

		doc := newTestDoc(9)
		landed, err = ApplyUpdateV1Landed(doc, EncodeStateAsUpdateV1(newRoom(), nil), nil)
		require.NoError(t, err)
		assert.Nil(t, landed)
	})

	t.Run("a failure that integrates nothing lands nil", func(t *testing.T) {
		room := newRoom()
		room.SetMaxPendingItems(10) // 4 parked and 7 more pass it
		landed, err := ApplyUpdateV1Landed(room, landedParking(t, 5, 7), nil)
		require.ErrorIs(t, err, ErrInvalidUpdate)
		assert.Nil(t, landed)
	})
}

// Applies that each park a struct and then fail, one per 20 parked structs,
// cost time linear in the applies: what a failed apply relays is read from
// what it parked, not from a pass over the queue.
func TestPerf_FailedApplyAfterAParkIsLinear(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	clock := threadCPUClock(t)
	// waiting is n structs of new clients from base, each after a struct of a
	// client the room never gets.
	waiting := func(n int, base ClientID) map[ClientID][]*Item {
		m := make(map[ClientID][]*Item, n)
		for i := range n {
			c := base + ClientID(i)
			m[c] = []*Item{{ID: ID{Client: c}, Origin: &ID{Client: 1 << 40}, Content: NewContentDeleted(1)}}
		}
		return m
	}
	fail := func(n int) time.Duration {
		k := n / 20
		failing := make([][]byte, k)
		for i := range failing {
			u := structsV1(waiting(1, ClientID(50_000_000+i)))
			u[len(u)-1] = 1 // a delete set of one client that the update cuts off
			failing[i] = u
		}
		room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
		require.NoError(t, ApplyUpdateV1(room, structsV1(waiting(n, 1_000_000)), nil))
		landed := make([][]byte, k)
		took := clock(func() {
			for i, u := range failing {
				var err error
				if landed[i], err = ApplyUpdateV1Landed(room, u, nil); err == nil {
					t.Fatal("the cut update applied")
				}
			}
		})
		for i := 0; i < k; i += 97 { // a sample keeps the test fast
			peer := New(WithClientID(2))
			require.NoError(t, ApplyUpdateV1(peer, landed[i], nil))
			require.Equal(t, 1, peer.PendingStats().Items, "update %d: the landed bytes carry its parked struct", i)
		}
		require.Equal(t, n+k, room.PendingStats().Items)
		return took
	}
	const small, large, runs = 2000, 32000, 16
	var s, l time.Duration = math.MaxInt64, math.MaxInt64
	for range 5 {
		var sum time.Duration
		for range runs {
			sum += fail(small)
		}
		s, l = min(s, sum), min(l, fail(large))
	}
	t.Logf("a failed apply after a park per 20 parked structs, as %d queues of %d: %v; as one of %d: %v (%.1f×)", runs, small, s, large, l, float64(l)/float64(s))
	if l > 3*s {
		t.Fatalf("failed applies grow with the queue: %v for one queue of %d, %v for %d queues of %d", l, large, s, runs, small)
	}
}
