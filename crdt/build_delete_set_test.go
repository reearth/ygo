package crdt

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// referenceBuildDeleteSet builds the delete set the plain way: add every
// deleted item, then sort and compact each client's ranges.
func referenceBuildDeleteSet(store *StructStore) DeleteSet {
	ds := newDeleteSet()
	for _, items := range store.clients {
		for _, item := range items {
			if item.Deleted {
				ds.add(item.ID, item.Content.Len())
			}
		}
	}
	for client := range ds.clients {
		ds.sortAndCompact(client)
	}
	return ds
}

// randomStore returns a store of up to five clients whose structs have random
// lengths and deletion flags, with an occasional clock gap between two
// structs.
func randomStore(r *rand.Rand) *StructStore {
	store := newStructStore()
	for c := range 1 + r.IntN(5) {
		var clock uint64
		for range r.IntN(40) {
			if r.IntN(8) == 0 {
				clock += uint64(1 + r.IntN(3))
			}
			n := 1 + r.IntN(4)
			store.Append(&Item{
				ID:      ID{Client: ClientID(c + 1), Clock: clock},
				Deleted: r.IntN(2) == 0,
				Content: NewContentDeleted(n),
			})
			clock += uint64(n)
		}
	}
	return store
}

func TestUnit_BuildDeleteSet_MatchesReferenceOnRandomStores(t *testing.T) {
	for seed := range uint64(2000) {
		store := randomStore(rand.New(rand.NewPCG(seed, 0)))
		require.Equal(t, referenceBuildDeleteSet(store), buildDeleteSet(store))
	}
}

// TestUnit_BuildDeleteSet_MatchesReferenceOnRandomDocs edits three peers'
// texts, arrays and maps, with moves and deletes, GC on and off, and syncs
// them in random order, checking every peer's delete set after each step.
func TestUnit_BuildDeleteSet_MatchesReferenceOnRandomDocs(t *testing.T) {
	for seed := range uint64(200) {
		r := rand.New(rand.NewPCG(seed, 0))
		gc := seed%2 == 0
		docs := make([]*Doc, 3)
		for i := range docs {
			docs[i] = New(WithClientID(ClientID(i+1)), WithGC(gc))
		}
		for range 60 {
			d := docs[r.IntN(len(docs))]
			if r.IntN(4) == 0 {
				src := docs[r.IntN(len(docs))]
				require.NoError(t, ApplyUpdateV1(d, EncodeStateAsUpdateV1(src, nil), nil))
			} else {
				editRandomly(r, d)
			}
			for _, doc := range docs {
				doc.mu.Lock()
				want, got := referenceBuildDeleteSet(doc.store), buildDeleteSet(doc.store)
				doc.mu.Unlock()
				require.Equal(t, want, got)
			}
		}
	}
}

func editRandomly(r *rand.Rand, d *Doc) {
	txt, arr, m := d.GetText("t"), d.GetArray("a"), d.GetMap("m")
	d.Transact(func(txn *Transaction) {
		for range 1 + r.IntN(3) {
			switch r.IntN(6) {
			case 0:
				txt.Insert(txn, r.IntN(txt.Len()+1), "xyz"[:1+r.IntN(3)], nil)
			case 1:
				if n := txt.Len(); n > 0 {
					i := r.IntN(n)
					txt.Delete(txn, i, 1+r.IntN(n-i))
				}
			case 2:
				arr.Insert(txn, r.IntN(arr.Len()+1), []any{r.IntN(100)})
			case 3:
				if n := arr.Len(); n > 0 {
					arr.Delete(txn, r.IntN(n), 1)
				}
			case 4:
				if n := arr.Len(); n > 1 {
					arr.Move(txn, r.IntN(n), r.IntN(n+1))
				}
			case 5:
				if r.IntN(3) == 0 {
					m.Delete(txn, "k")
				} else {
					m.Set(txn, "k", r.IntN(100))
				}
			}
		}
	})
}

// TestUnit_BuildDeleteSet_AllocatesOnlyTheResult checks that building a delete
// set costs no more allocations than appending its ranges to a new set.
func TestUnit_BuildDeleteSet_AllocatesOnlyTheResult(t *testing.T) {
	const items = 64
	store := newStructStore()
	for clock := range uint64(items) {
		store.Append(&Item{ID: ID{Client: 1, Clock: clock}, Deleted: clock%2 == 0, Content: NewContentString("a")})
	}
	var sink DeleteSet
	want := testing.AllocsPerRun(100, func() {
		ds := newDeleteSet()
		var ranges []DeleteRange
		for clock := uint64(0); clock < items; clock += 2 {
			ranges = append(ranges, DeleteRange{Clock: clock, Len: 1})
		}
		ds.clients[1] = ranges
		sink = ds
	})
	got := testing.AllocsPerRun(100, func() { sink = buildDeleteSet(store) })
	require.Equal(t, referenceBuildDeleteSet(store), sink)
	require.LessOrEqual(t, int(got), int(want))
}
