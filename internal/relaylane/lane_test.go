package relaylane_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/internal/relaylane"
)

// syncUpdates returns n real V1 update blobs, each one Transact's worth of
// edits on a fresh doc, plus the final text those updates reconstruct.
// Inserting at index 0 avoids needing a length accessor and keeps the result
// deterministic.
func syncUpdates(t *testing.T, n int) (updates [][]byte, wantText string) {
	t.Helper()
	src := crdt.New()
	txt := src.GetText("t")
	unsub := src.OnUpdate(func(u []byte, _ any) {
		updates = append(updates, append([]byte(nil), u...))
	})
	for i := 0; i < n; i++ {
		src.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
	}
	unsub()
	require.Len(t, updates, n, "each Transact must produce exactly one update")
	return updates, txt.ToString()
}

// applyAll applies every blob to a fresh doc and returns the resulting text.
func applyAll(t *testing.T, blobs [][]byte) string {
	t.Helper()
	got := crdt.New()
	for _, b := range blobs {
		require.NoError(t, crdt.ApplyUpdateV1(got, b, nil))
	}
	return got.GetText("t").ToString()
}

// A single queued update must come back byte-identical: the fast path must
// not pay for a merge it does not need.
func TestLane_SingleSync_NoMerge(t *testing.T) {
	updates, _ := syncUpdates(t, 1)
	l := relaylane.New(4)

	l.Push(cluster.KindSync, updates[0])

	got, ok := l.TakeSync()
	require.True(t, ok)
	require.Equal(t, updates[0], got)
	require.Zero(t, l.Stats().Coalesced, "one entry must not count as coalesced")
}

// Overflowing the lane must merge rather than drop or block: every update
// must still be represented in what comes out.
func TestLane_OverflowCoalesces_NoLoss(t *testing.T) {
	const n = 20
	updates, wantText := syncUpdates(t, n)
	l := relaylane.New(2) // far smaller than n, so overflow is guaranteed

	for _, u := range updates {
		l.Push(cluster.KindSync, u) // must never block
	}

	require.NotZero(t, l.Stats().Coalesced, "overflow must have coalesced")
	require.Zero(t, l.Stats().HardDrops, "coalescing must not drop")

	var out [][]byte
	for {
		b, ok := l.TakeSync()
		if !ok {
			break
		}
		out = append(out, b)
	}
	require.Less(t, len(out), n, "coalescing must reduce the entry count")
	require.Equal(t, wantText, applyAll(t, out), "no update may be lost")
}

// Awareness is keep-latest: a newer entry supersedes an unread older one.
// Awareness is idempotent heartbeat state, so the superseded entry self-heals
// within one heartbeat interval.
func TestLane_Awareness_KeepsLatest(t *testing.T) {
	l := relaylane.New(4)

	l.Push(cluster.KindAwareness, []byte{0x01})
	l.Push(cluster.KindAwareness, []byte{0x02})

	got, ok := l.TakeAwareness()
	require.True(t, ok)
	require.Equal(t, []byte{0x02}, got)
	require.Equal(t, uint64(1), l.Stats().AwarenessSuperseded)

	_, ok = l.TakeAwareness()
	require.False(t, ok, "the slot must be empty after being taken")
}

// The two kinds must never be merged into each other.
func TestLane_KindsNeverMix(t *testing.T) {
	updates, _ := syncUpdates(t, 1)
	l := relaylane.New(4)

	l.Push(cluster.KindAwareness, []byte{0xAA})
	l.Push(cluster.KindSync, updates[0])

	gotSync, ok := l.TakeSync()
	require.True(t, ok)
	require.Equal(t, updates[0], gotSync)

	gotAw, ok := l.TakeAwareness()
	require.True(t, ok)
	require.Equal(t, []byte{0xAA}, gotAw)
}

// A garbage blob makes MergeUpdatesV1 fail. The lane must fall back to
// one-at-a-time delivery rather than losing the batch.
func TestLane_MergeFailure_DoesNotLose(t *testing.T) {
	l := relaylane.New(1)

	l.Push(cluster.KindSync, []byte{0xFF, 0xFF, 0xFF})
	l.Push(cluster.KindSync, []byte{0xFE, 0xFE, 0xFE})

	var count int
	for {
		if _, ok := l.TakeSync(); !ok {
			break
		}
		count++
	}
	require.Equal(t, 2, count, "both entries must survive a failed merge")
	require.Zero(t, l.Stats().HardDrops)
}

// Push must make the lane readable so a worker parked on Signal wakes up.
func TestLane_Push_Signals(t *testing.T) {
	l := relaylane.New(4)
	require.True(t, l.Empty())

	l.Push(cluster.KindSync, []byte{0x01})

	select {
	case <-l.Signal():
	default:
		t.Fatal("Push must signal a waiting worker")
	}
	require.False(t, l.Empty())
}

// Depth must track what the lane actually holds, in both directions, and must
// count the two kinds independently — a producer reading it to gauge
// saturation is misled by either a stuck or an over-counted number.
//
// Real V1 blobs, and a cap well above n, so nothing here coalesces: Depth is
// being asserted against a known queue length, not against whatever a merge
// left behind.
func TestLane_Depth_RisesWithPushAndFallsWithTake(t *testing.T) {
	const n = 3
	updates, _ := syncUpdates(t, n)
	l := relaylane.New(n + 5)
	require.Zero(t, l.Depth(), "a fresh lane holds nothing")

	for i, u := range updates {
		l.Push(cluster.KindSync, u)
		require.Equal(t, i+1, l.Depth(), "each sync push adds exactly one")
	}

	// Awareness is a separate, single slot: the first push adds one, and a
	// second replaces rather than adds.
	l.Push(cluster.KindAwareness, []byte{0xaa})
	require.Equal(t, n+1, l.Depth(), "a pending awareness blob counts as one")
	l.Push(cluster.KindAwareness, []byte{0xbb})
	require.Equal(t, n+1, l.Depth(), "awareness is latest-only, so it cannot stack")

	// TakeSync drains the WHOLE sync backlog as one merged blob, so it drops
	// the sync contribution to zero in a single call.
	_, ok := l.TakeSync()
	require.True(t, ok)
	require.Equal(t, 1, l.Depth(), "only the awareness blob is left")

	_, ok = l.TakeAwareness()
	require.True(t, ok)
	require.Zero(t, l.Depth(), "a fully drained lane holds nothing")
}

// Full must flip exactly one push before a merge would happen, so a producer
// that respects it never pays for a coalesce, and must clear once the queue is
// drained.
//
// Asserted against Stats().Coalesced rather than against an arithmetic
// restatement of the threshold: the promise is "the next push would merge",
// and only the merge counter can witness that.
func TestLane_Full_PredictsTheNextMerge(t *testing.T) {
	const capacity = 3
	updates, _ := syncUpdates(t, capacity+1)
	l := relaylane.New(capacity)

	for i := 0; i < capacity; i++ {
		require.False(t, l.Full(), "a lane below capacity is not full (after %d pushes)", i)
		l.Push(cluster.KindSync, updates[i])
	}
	require.True(t, l.Full(), "at capacity, the next sync push must merge")
	require.Zero(t, l.Stats().Coalesced, "nothing has merged yet")

	// Awareness must not affect the verdict: it is latest-only and is not
	// governed by the cap. Shaped to be one short of the cap on the SYNC
	// queue with awareness also pending, so a Full() that summed the two
	// kinds — the obvious wrong implementation, and the one this change's
	// brief proposed — would report full here while the next sync push still
	// would not merge.
	l2 := relaylane.New(capacity)
	for i := 0; i < capacity-1; i++ {
		l2.Push(cluster.KindSync, updates[i])
	}
	l2.Push(cluster.KindAwareness, []byte{0xaa})
	require.Equal(t, capacity, l2.Depth(), "the two kinds together do reach the cap")
	require.False(t, l2.Full(), "a pending awareness blob is not sync capacity")
	l2.Push(cluster.KindSync, updates[capacity-1])
	require.Zero(t, l2.Stats().Coalesced, "and indeed that push did not merge")

	// Taking the honest-but-ignored path proves Full was telling the truth.
	l.Push(cluster.KindSync, updates[capacity])
	require.NotZero(t, l.Stats().Coalesced, "the push Full warned about did merge")

	_, ok := l.TakeSync()
	require.True(t, ok)
	require.False(t, l.Full(), "a drained lane is not full")
}
