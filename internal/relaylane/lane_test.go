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
