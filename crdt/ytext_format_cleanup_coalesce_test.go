package crdt

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fragmentedDeleteTime times applying, to a peer with formatted text, one
// remote deletion of n adjacent single-character structs.
func fragmentedDeleteTime(tb testing.TB, n int) time.Duration {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "ab", nil) })
	for range n {
		a.Transact(func(txn *Transaction) { at.Insert(txn, 1, "x", nil) })
	}
	require.NoError(tb, ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, nil), nil))
	b.Transact(func(txn *Transaction) { bt.Format(txn, n+1, 1, Attributes{"bold": true}) })
	sv := a.StateVector()
	a.Transact(func(txn *Transaction) { at.Delete(txn, 1, n) })
	u := EncodeStateAsUpdateV1(a, sv)
	start := time.Now()
	require.NoError(tb, ApplyUpdateV1(b, u, nil))
	return time.Since(start)
}

func TestPerf_RemoteFragmentedDeleteCleanupIsLinear(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	const small, large = 1000, 8000 // 8×: linear ≈ 8×, quadratic ≈ 64×
	var s, l time.Duration
	for range 3 {
		s += fragmentedDeleteTime(t, small)
		l += fragmentedDeleteTime(t, large)
	}
	t.Logf("%v → %v", s, l)
	require.Less(t, l, 24*s+20*time.Millisecond, "the contextless cleanup grows superlinearly")
}

func BenchmarkYText_RemoteFragmentedDelete(b *testing.B) {
	for _, n := range []int{1000, 8000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var d time.Duration
			for range b.N {
				d += fragmentedDeleteTime(b, n)
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
