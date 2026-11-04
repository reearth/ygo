package crdt

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fragmentedDeleteTimes measures with clock, for each of sizes, applying to k
// peers with formatted text one remote deletion each of n adjacent
// single-character structs. The sizes are measured back to back, so they see
// the machine alike.
func fragmentedDeleteTimes(tb testing.TB, clock func(func()) time.Duration, sizes ...setSize) []time.Duration {
	peers, updates := make([][]*Doc, len(sizes)), make([][][]byte, len(sizes))
	for s, size := range sizes {
		peers[s], updates[s] = make([]*Doc, size.k), make([][]byte, size.k)
		for i := range size.k {
			a, b := newTestDoc(1), newTestDoc(2)
			at, bt := a.GetText("t"), b.GetText("t")
			a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "ab", nil) })
			for range size.n {
				a.Transact(func(txn *Transaction) { at.Insert(txn, 1, "x", nil) })
			}
			require.NoError(tb, ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, nil), nil))
			b.Transact(func(txn *Transaction) { bt.Format(txn, size.n+1, 1, Attributes{"bold": true}) })
			sv := a.StateVector()
			a.Transact(func(txn *Transaction) { at.Delete(txn, 1, size.n) })
			peers[s][i], updates[s][i] = b, EncodeStateAsUpdateV1(a, sv)
		}
	}
	took := make([]time.Duration, len(sizes))
	var errs []error
	for s := range sizes {
		took[s] = clock(func() {
			for i, b := range peers[s] {
				if err := ApplyUpdateV1(b, updates[s][i], nil); err != nil {
					errs = append(errs, err)
				}
			}
		})
	}
	require.Empty(tb, errs)
	return took
}

func TestPerf_RemoteFragmentedDeleteCleanupIsLinear(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	// Thread CPU time, not wall time, so waits for a core on a loaded machine
	// do not count. Sixteen small deletions hold as many structs as one large
	// one, so a struct costs about the same in both when the cleanup is linear,
	// and sixteen times as much in the large one when it is quadratic. Each
	// size keeps the fewest of interleaved runs.
	clock := threadCPUClock(t)
	const small, large, peers = 500, 8000, 16
	var s, l time.Duration = math.MaxInt64, math.MaxInt64
	for range 16 {
		took := fragmentedDeleteTimes(t, clock, setSize{small, peers}, setSize{large, 1})
		s, l = min(s, took[0]), min(l, took[1])
	}
	t.Logf("%d structs deleted as %d deletions of %d, then as one: %v → %v (%.1f×)", large, peers, small, s, l, float64(l)/float64(s))
	require.Less(t, l, 3*s, "the contextless cleanup grows superlinearly")
}

func BenchmarkYText_RemoteFragmentedDelete(b *testing.B) {
	for _, n := range []int{1000, 8000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var d time.Duration
			for range b.N {
				d += fragmentedDeleteTimes(b, wallTime, setSize{n, 1})[0]
			}
			b.ReportMetric(float64(d.Nanoseconds())/float64(b.N), "apply-ns/op")
		})
	}
}

// refCleanupYTextAfterTransaction is the cleanup without coalescing: every
// deleted item cleans its own gap.
func refCleanupYTextAfterTransaction(remote, txn *Transaction) {
	store := txn.doc.store
	var full []*abstractType
	needFull := map[*abstractType]bool{}
	addFull := func(t *abstractType) {
		if !needFull[t] {
			needFull[t] = true
			full = append(full, t)
		}
	}
	for _, client := range sortedStateClients(remote.afterState) {
		storeRange(store, client, remote.beforeState.Clock(client), remote.afterState.Clock(client), func(it *Item) {
			if !it.Deleted && isContentFormat(it) && it.Parent != nil {
				addFull(it.Parent)
			}
		})
	}
	ds := remote.deleteSet
	for _, client := range ds.orderedClients() {
		sorted := DeleteSet{clients: map[ClientID][]DeleteRange{client: slices.Clone(ds.clients[client])}}
		sorted.sortAndCompact(client)
		for _, r := range sorted.clients[client] {
			storeRange(store, client, r.Clock, r.Clock+r.Len, func(it *Item) {
				p := it.Parent
				if p == nil || !p.cleanFormatting || needFull[p] {
					return
				}
				if isContentFormat(it) {
					addFull(p)
					return
				}
				item := it
				for item.Right != nil && (item.Right.Deleted || !item.Right.Content.IsCountable()) {
					item = item.Right
				}
				seen := map[string]bool{}
				for item != nil && (item.Deleted || !item.Content.IsCountable()) {
					if cf, ok := item.Content.(*ContentFormat); ok && !item.Deleted {
						if seen[cf.Key] {
							item.delete(txn)
						} else {
							seen[cf.Key] = true
						}
					}
					item = item.Left
				}
			})
		}
	}
	for _, t := range full {
		cleanupYTextFormatting(txn, t)
	}
}

// Coalescing the contextless cleanup per gap deletes the same markers in the
// same order as cleaning every deleted item's gap.
func TestUnit_YText_FormatCleanup_CoalescedMatchesPerItem(t *testing.T) {
	seeds := 300
	if raceEnabled || testing.Short() {
		seeds = 20
	}
	keys := []string{"b", "i"}
	vals := []any{true, "x", nil}
	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		attrs := func() Attributes {
			return Attributes{keys[rng.Intn(2)]: vals[rng.Intn(3)]}
		}
		edit := func(d *Doc, n int) {
			txt := d.GetText("t")
			for range n {
				d.Transact(func(txn *Transaction) {
					l := txt.Len()
					switch r := rng.Intn(10); {
					case r < 5 || l < 2:
						var a Attributes
						if rng.Intn(2) == 0 {
							a = attrs()
						}
						txt.Insert(txn, rng.Intn(l+1), "xy"[:1+rng.Intn(2)], a)
					case r < 8:
						i := rng.Intn(l)
						txt.Format(txn, i, 1+rng.Intn(l-i), attrs())
					default:
						i := rng.Intn(l)
						txt.Delete(txn, i, 1+rng.Intn(min(3, l-i)))
					}
				})
			}
		}
		// B formats while A, concurrently, deletes, so A's deletions land in
		// gaps holding B's markers.
		a, b := newTestDoc(1), newTestDoc(2)
		edit(a, 14)
		syncText(t, a, b, nil)
		sv := a.StateVector()
		bt, at := b.GetText("t"), a.GetText("t")
		for range 4 {
			b.Transact(func(txn *Transaction) {
				if l := bt.Len(); l > 0 {
					i := rng.Intn(l)
					bt.Format(txn, i, 1+rng.Intn(l-i), attrs())
				}
			})
		}
		for range 1 + rng.Intn(6) {
			a.Transact(func(txn *Transaction) {
				if l := at.Len(); l > 0 {
					i := rng.Intn(l)
					at.Delete(txn, i, 1+rng.Intn(min(3, l-i)))
				}
			})
		}
		u := EncodeStateAsUpdateV1(a, sv)
		state := EncodeStateAsUpdateV1(b, nil)

		run := func(cleanup func(remote, txn *Transaction)) (*Doc, DeleteSet) {
			d := newTestDoc(3)
			d.GetText("t")
			require.NoError(t, ApplyUpdateV1(d, state, nil))
			var ds DeleteSet
			d.Transact(func(remote *Transaction) {
				require.NoError(t, applyV1Txn(remote, u))
				remote.afterState = d.store.StateVector()
				txn := &Transaction{doc: d, Local: true, deleteSet: newOrderedDeleteSet(),
					changed: map[*abstractType]map[string]struct{}{}, ctx: context.Background()}
				cleanup(remote, txn)
				ds = txn.deleteSet
			})
			return d, ds
		}
		got, gotDS := run(cleanupYTextAfterTransaction)
		want, wantDS := run(refCleanupYTextAfterTransaction)
		require.Equal(t, wantDS.orderedClients(), gotDS.orderedClients(), "seed %d", seed)
		require.Equal(t, wantDS.clients, gotDS.clients, "seed %d", seed)
		require.Equal(t, EncodeStateAsUpdateV1(want, nil), EncodeStateAsUpdateV1(got, nil), "seed %d", seed)
	}
}
