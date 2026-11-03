package crdt

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"
)

// A seq holds what a slice holds under random inserts, cuts and splices,
// short and past seqFlat, where it is a treap that reuses the nodes it cut.
func TestUnit_Seq_StepsMatchASlice(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for round := range 30 {
		var s seq[int]
		var want []int
		limit := 1 + rng.IntN(6*seqFlat)
		for step := range 2000 {
			from := rng.IntN(len(want) + 1)
			switch rng.IntN(3) {
			case 0:
				if len(want) < limit {
					v := rng.Int()
					s.insert(from, v)
					want = slices.Insert(want, from, v)
				}
			case 1:
				most := 8
				if rng.IntN(64) == 0 {
					most = 2 * seqFlat // a cut of many nodes at once
				}
				to := from + rng.IntN(min(len(want)-from, most)+1)
				s.replace(from, to, nil)
				want = slices.Delete(want, from, to)
			default:
				to := from + rng.IntN(min(len(want)-from, 8)+1)
				most := 16
				if rng.IntN(64) == 0 {
					most = 2 * seqFlat // a splice that takes a short seq past seqFlat
				}
				vals := make([]int, rng.IntN(min(max(limit-len(want), 0), most)+1))
				for i := range vals {
					vals[i] = rng.Int()
				}
				s.replace(from, to, vals)
				want = slices.Replace(want, from, to, vals...)
			}
			if s.len() != len(want) {
				t.Fatalf("round %d step %d: len %d, want %d", round, step, s.len(), len(want))
			}
			if len(want) > 0 {
				i := rng.IntN(len(want))
				if s.at(i) != want[i] {
					t.Fatalf("round %d step %d: at(%d) = %d, want %d", round, step, i, s.at(i), want[i])
				}
				lo := rng.IntN(len(want))
				hi := lo + rng.IntN(min(len(want)-lo, 64)+1)
				var got []int
				s.each(lo, hi, func(v int) { got = append(got, v) })
				if !slices.Equal(got, want[lo:hi]) {
					t.Fatalf("round %d step %d: each(%d, %d) gives %v, want %v", round, step, lo, hi, got, want[lo:hi])
				}
			}
		}
		if !slices.Equal(s.slice(), want) {
			t.Fatalf("round %d: the seq holds %v, want %v", round, s.slice(), want)
		}
	}
}

// A sorted seq finds by search what a binary search of the slice finds, and
// takes sorted batches, one by one or in one merge, each after the elements
// equal to it.
func TestUnit_Seq_SortedMergesMatchASlice(t *testing.T) {
	type kv struct{ key, id int }
	byKey := func(a, b kv) int { return cmp.Compare(a.key, b.key) }
	rng := rand.New(rand.NewPCG(3, 4))
	id, rounds := 0, 20
	if raceEnabled {
		rounds = 3 // the race detector makes each round about ten times slower
	}
	for round := range rounds {
		var s seq[kv]
		var want []kv
		keys := 1 + rng.IntN(8*seqFlat)
		for step := range 150 {
			add := make([]kv, rng.IntN(1+rng.IntN(3*seqFlat)))
			for i := range add {
				id++
				add[i] = kv{rng.IntN(keys), id}
			}
			slices.SortStableFunc(add, byKey)
			want = append(want, add...)
			slices.SortStableFunc(want, byKey)
			s.mergeIn(add, byKey)
			if !slices.Equal(s.slice(), want) {
				t.Fatalf("round %d step %d: the seq holds %v, want %v", round, step, s.slice(), want)
			}
			k := rng.IntN(keys)
			if got, at := s.search(func(v kv) bool { return v.key >= k }), sort.Search(len(want), func(i int) bool { return want[i].key >= k }); got != at {
				t.Fatalf("round %d step %d: search(>= %d) = %d, want %d", round, step, k, got, at)
			}
			if len(want) > 0 && rng.IntN(3) == 0 {
				from := rng.IntN(len(want))
				to := from + rng.IntN(len(want)-from+1)
				s.replace(from, to, nil)
				want = slices.Delete(want, from, to)
			}
		}
	}
}
