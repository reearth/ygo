package crdt

import (
	"fmt"
	"math"
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

// setSize is k sets of n.
type setSize struct{ n, k int }

// moveDeleteSets are k sets of n winning moves, ready for three deletes: a
// local cascade delete of their nested array, that delete applied on a peer
// (the moves' client sorts first, so a remote apply may tombstone them before
// their parent), and undoing n moves in a live array.
type moveDeleteSets struct {
	docs, peers []*Doc
	svs         []StateVector
	roots, arrs []*YArray
	ums         []*UndoManager
	updates     [][]byte
	errs        []error
	undone      []bool
}

func newMoveDeleteSets(t testing.TB, size setSize) *moveDeleteSets {
	n, k := size.n, size.k
	s := &moveDeleteSets{
		docs: make([]*Doc, k), peers: make([]*Doc, k), svs: make([]StateVector, k),
		roots: make([]*YArray, k), arrs: make([]*YArray, k), ums: make([]*UndoManager, k),
		updates: make([][]byte, k), errs: make([]error, k), undone: make([]bool, k),
	}
	for i := range k {
		s.docs[i], s.peers[i] = movedNested(t, n, 5, 3), newTestDoc(9)
		require.NoError(t, s.peers[i].ApplyUpdate(s.docs[i].EncodeStateAsUpdate()))
		s.svs[i], s.roots[i] = s.peers[i].StateVector(), s.docs[i].GetArray("a")

		ud := newTestDoc(1)
		arr := ud.GetArray("a")
		ud.Transact(func(txn *Transaction) {
			vals := make([]any, n)
			for i := range vals {
				vals[i] = i
			}
			arr.Push(txn, vals)
		})
		s.ums[i] = NewUndoManager(ud, []SharedType{arr})
		ud.Transact(func(txn *Transaction) {
			for i := 0; i < n; i++ {
				arr.Move(txn, 0, n)
			}
		})
		s.arrs[i] = arr
	}
	return s
}

// moveDeleteTimes measures with clock the three deletes on each of sizes. Each
// delete runs on every size back to back, so they see the machine alike.
func moveDeleteTimes(t testing.TB, clock func(func()) time.Duration, sizes ...setSize) (local, remote, undo []time.Duration) {
	all := make([]*moveDeleteSets, len(sizes))
	for i, size := range sizes {
		all[i] = newMoveDeleteSets(t, size)
	}
	for _, s := range all {
		local = append(local, clock(func() {
			for i, doc := range s.docs {
				doc.Transact(func(txn *Transaction) { s.roots[i].Delete(txn, 0, 1) })
			}
		}))
	}
	for _, s := range all {
		for i, doc := range s.docs {
			s.updates[i] = EncodeStateAsUpdateV1(doc, s.svs[i])
		}
	}
	for _, s := range all {
		remote = append(remote, clock(func() {
			for i, peer := range s.peers {
				s.errs[i] = peer.ApplyUpdate(s.updates[i])
			}
		}))
	}
	for _, s := range all {
		undo = append(undo, clock(func() {
			for i, um := range s.ums {
				s.undone[i] = um.Undo()
			}
		}))
	}
	for _, s := range all {
		for i, peer := range s.peers {
			require.NoError(t, s.errs[i])
			require.Equal(t, 0, peer.GetArray("a").Len())
			require.True(t, s.undone[i])
			require.EqualValues(t, 0, s.arrs[i].Get(0))
		}
	}
	return local, remote, undo
}

// Tombstoning many winning moves must not scan the parent list once per move.
func TestPerf_DeletingWinningMovesIsLinear(t *testing.T) {
	// Instrumented builds distort the ratio and run for minutes; the
	// benchmark below tracks this in CI.
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	// Thread CPU time, not wall time, so waits for a core on a loaded machine
	// do not count. Eight small sets hold as many moves as one large set, so a
	// move costs the same in both when the work is linear, and eight times as
	// much in the large set when it is quadratic. Each size keeps the fewest of
	// interleaved runs, since the remote apply's client order is randomised.
	clock := threadCPUClock(t)
	const small, large, sets = 250, 2000, 8
	var sl, sr, su, ll, lr, lu time.Duration = math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64
	for range 8 {
		l, r, u := moveDeleteTimes(t, clock, setSize{small, sets}, setSize{large, 1})
		sl, sr, su = min(sl, l[0]), min(sr, r[0]), min(su, u[0])
		ll, lr, lu = min(ll, l[1]), min(lr, r[1]), min(lu, u[1])
	}
	t.Logf("%d moves as %d sets of %d, then as one set: local %v → %v (%.1f×), remote %v → %v (%.1f×), undo %v → %v (%.1f×)", large, sets, small,
		sl, ll, float64(ll)/float64(sl), sr, lr, float64(lr)/float64(sr), su, lu, float64(lu)/float64(su))
	require.Less(t, ll, 3*sl, "local cascade delete grows superlinearly")
	require.Less(t, lr, 3*sr, "remote cascade delete grows superlinearly")
	require.Less(t, lu, 3*su, "undoing moves grows superlinearly")
}

func wallTime(f func()) time.Duration {
	start := time.Now()
	f()
	return time.Since(start)
}

func BenchmarkYArray_DeleteWinningMoves(b *testing.B) {
	for _, n := range []int{1000, 8000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var l, r, u time.Duration
			for i := 0; i < b.N; i++ {
				a, c, d := moveDeleteTimes(b, wallTime, setSize{n, 1})
				l, r, u = l+a[0], r+c[0], u+d[0]
			}
			b.ReportMetric(float64(l.Nanoseconds())/float64(b.N), "local-ns/op")
			b.ReportMetric(float64(r.Nanoseconds())/float64(b.N), "remote-ns/op")
			b.ReportMetric(float64(u.Nanoseconds())/float64(b.N), "undo-ns/op")
		})
	}
}
