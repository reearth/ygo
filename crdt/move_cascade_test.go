package crdt

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// movedNested returns a doc (client owner) whose root array holds one nested
// array of n elements, each moved by client mover (so each target owns a
// winning ContentMove).
func movedNested(t testing.TB, n int, owner, mover uint64) *Doc {
	doc := newTestDoc(owner)
	root := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		inner := NewArrayPrelim()
		vals := make([]any, n)
		for i := range vals {
			vals[i] = i
		}
		inner.Push(txn, vals)
		root.PushType(txn, inner)
	})
	m := newTestDoc(mover)
	require.NoError(t, m.ApplyUpdate(doc.EncodeStateAsUpdate()))
	inner := m.GetArray("a").Get(0).(*YArray)
	m.Transact(func(txn *Transaction) {
		for i := 0; i < n; i++ {
			inner.Move(txn, 0, n)
		}
	})
	require.NoError(t, doc.ApplyUpdate(EncodeStateAsUpdateV1(m, doc.StateVector())))
	return doc
}

// moveDeleteTimes times three deletes of n winning moves: a local cascade
// delete of their nested array, that delete applied on a peer (the moves'
// client sorts first, so a remote apply may tombstone them before their
// parent), and undoing n moves in a live array.
func moveDeleteTimes(t testing.TB, n int) (local, remote, undo time.Duration) {
	doc := movedNested(t, n, 5, 3)
	peer := newTestDoc(9)
	require.NoError(t, peer.ApplyUpdate(doc.EncodeStateAsUpdate()))
	sv := peer.StateVector()
	root := doc.GetArray("a")

	start := time.Now()
	doc.Transact(func(txn *Transaction) { root.Delete(txn, 0, 1) })
	local = time.Since(start)
	update := EncodeStateAsUpdateV1(doc, sv)
	start = time.Now()
	require.NoError(t, peer.ApplyUpdate(update))
	remote = time.Since(start)
	require.Equal(t, 0, peer.GetArray("a").Len())

	ud := newTestDoc(1)
	arr := ud.GetArray("a")
	ud.Transact(func(txn *Transaction) {
		vals := make([]any, n)
		for i := range vals {
			vals[i] = i
		}
		arr.Push(txn, vals)
	})
	um := NewUndoManager(ud, []SharedType{arr})
	ud.Transact(func(txn *Transaction) {
		for i := 0; i < n; i++ {
			arr.Move(txn, 0, n)
		}
	})
	start = time.Now()
	require.True(t, um.Undo())
	undo = time.Since(start)
	require.EqualValues(t, 0, arr.Get(0))
	return local, remote, undo
}

// Tombstoning many winning moves must not scan the parent list once per move.
func TestPerf_DeletingWinningMovesIsLinear(t *testing.T) {
	// Instrumented builds distort the ratio and run for minutes; the
	// benchmark below tracks this in CI.
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	// Summed over runs: the remote apply's client order is randomised.
	total := func(n int) (l, r, u time.Duration) {
		for i := 0; i < 4; i++ {
			a, b, c := moveDeleteTimes(t, n)
			l, r, u = l+a, r+b, u+c
		}
		return
	}
	const small, large = 500, 4000 // 8×: linear ≈ 8×, quadratic ≈ 64×
	sl, sr, su := total(small)
	ll, lr, lu := total(large)
	t.Logf("local %v → %v, remote %v → %v, undo %v → %v", sl, ll, sr, lr, su, lu)
	slack := 20 * time.Millisecond
	require.Less(t, ll, 24*sl+slack, "local cascade delete grows superlinearly")
	require.Less(t, lr, 24*sr+slack, "remote cascade delete grows superlinearly")
	require.Less(t, lu, 24*su+slack, "undoing moves grows superlinearly")
}

func BenchmarkYArray_DeleteWinningMoves(b *testing.B) {
	for _, n := range []int{1000, 8000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var l, r, u time.Duration
			for i := 0; i < b.N; i++ {
				a, c, d := moveDeleteTimes(b, n)
				l, r, u = l+a, r+c, u+d
			}
			b.ReportMetric(float64(l.Nanoseconds())/float64(b.N), "local-ns/op")
			b.ReportMetric(float64(r.Nanoseconds())/float64(b.N), "remote-ns/op")
			b.ReportMetric(float64(u.Nanoseconds())/float64(b.N), "undo-ns/op")
		})
	}
}
