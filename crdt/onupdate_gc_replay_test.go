package crdt

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

// The updates OnUpdate emits, encoded after the GC, rebuild the document: a
// replica that applies each of them in order, as a relay's peer or a store
// replaying its log does, ends with the content of the document that emitted
// them. Writer a inserts and deletes in the same transactions and formats its
// text; reader b applies a's updates with a text observer, so the format
// cleanup runs, and emits updates of its own.
func TestUnit_Doc_OnUpdateReplayRebuildsDocument(t *testing.T) {
	seeds := 200
	if raceEnabled || testing.Short() {
		seeds = 20
	}
	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		a, b := New(WithClientID(1)), New(WithClientID(2))
		at, am, aa := a.GetText("t"), a.GetMap("m"), a.GetArray("a")
		b.GetText("t").Observe(func(YTextEvent) {})
		var fromA, fromB [][]byte
		a.OnUpdate(func(u []byte, _ any) {
			fromA = append(fromA, u)
			require.NoError(t, ApplyUpdateV1(b, u, nil))
		})
		b.OnUpdate(func(u []byte, _ any) { fromB = append(fromB, u) })
		for range 30 {
			a.Transact(func(txn *Transaction) {
				for range 1 + rng.Intn(4) {
					l := at.Len()
					switch r := rng.Intn(8); {
					case r < 3 || l == 0:
						var attrs Attributes
						if rng.Intn(3) == 0 {
							attrs = Attributes{"bold": rng.Intn(2) == 0}
						}
						at.Insert(txn, rng.Intn(l+1), "xyz"[:1+rng.Intn(3)], attrs)
					case r < 5:
						i := rng.Intn(l)
						at.Delete(txn, i, 1+rng.Intn(l-i))
					case r < 6:
						i := rng.Intn(l)
						at.Format(txn, i, 1+rng.Intn(l-i), Attributes{"bold": true})
					case r < 7:
						am.Set(txn, "k", rng.Intn(10))
					default:
						aa.Insert(txn, 0, []any{rng.Intn(10)})
						if rng.Intn(2) == 0 {
							aa.Delete(txn, 0, 1)
						}
					}
				}
			})
		}
		for _, tc := range []struct {
			name    string
			src     *Doc
			updates [][]byte
		}{{"a", a, fromA}, {"b", b, fromB}} {
			replica := New(WithClientID(3))
			for _, u := range tc.updates {
				require.NoError(t, ApplyUpdateV1(replica, u, nil), "seed %d %s", seed, tc.name)
			}
			require.Equal(t, tc.src.GetText("t").ToDelta(), replica.GetText("t").ToDelta(), "seed %d %s", seed, tc.name)
			require.Equal(t, tc.src.GetMap("m").Entries(), replica.GetMap("m").Entries(), "seed %d %s", seed, tc.name)
			require.Equal(t, tc.src.GetArray("a").ToSlice(), replica.GetArray("a").ToSlice(), "seed %d %s", seed, tc.name)
			require.Equal(t, tc.src.StateVector(), replica.StateVector(), "seed %d %s", seed, tc.name)
		}
	}
}
