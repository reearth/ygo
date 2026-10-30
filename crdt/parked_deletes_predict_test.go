package crdt

import (
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// What checkParkedDeletes counts before the apply changes anything
// (parkedPartsOf of the update's delete set at structClocksOf) is what the
// apply then parks. Three writers edit a text, a map and
// an array nested in it, syncing at random; a receiver holds a state one
// writer had, and is sent the whole document encoded from a state vector that
// claims, for each client, a random clock between what the receiver holds and
// what the document holds. The update then lacks structs the receiver needs:
// structs after a gap, items whose origin, right origin or parent lies in it,
// and the delete ranges of what they hold park, while structs whose
// dependencies come later in the update, in another client's group,
// integrate. With the cap lifted, the ranges the apply parks are those
// checkParkedDeletes named, in V1 and in V2.
func TestUnit_ApplyUpdate_TheParkedDeletesCheckedAreWhatTheApplyParks(t *testing.T) {
	const rounds = 1000
	rng := rand.New(rand.NewPCG(334, 2526))
	parkedSome := 0
	for round := range rounds {
		writers := []*Doc{New(WithClientID(1)), New(WithClientID(2)), New(WithClientID(3))}
		const edits = 30
		snapshots := make([][]byte, 0, edits)
		for range edits {
			w := writers[rng.IntN(len(writers))]
			text, root := w.GetText("t"), w.GetMap("m")
			n := text.Len()
			var list *YArray
			if v, ok := root.Get("list"); ok {
				list, _ = v.(*YArray)
			}
			l := 0
			if list != nil {
				l = list.Len()
			}
			w.Transact(func(txn *Transaction) {
				switch op := rng.IntN(5); {
				case op == 0 && n > 0:
					pos := rng.IntN(n)
					text.Delete(txn, pos, 1+rng.IntN(min(3, n-pos)))
				case op == 1:
					root.Set(txn, string(rune('a'+rng.IntN(4))), rng.IntN(100))
				case op == 2 && list == nil:
					root.Set(txn, "list", NewArrayPrelim())
				case op == 2 && l > 0 && rng.IntN(2) == 0:
					list.Delete(txn, rng.IntN(l), 1)
				case op == 2:
					list.Insert(txn, rng.IntN(l+1), []any{rng.IntN(10)})
				default:
					text.Insert(txn, rng.IntN(n+1), string(rune('a'+rng.IntN(26)))+"x", nil)
				}
			})
			if rng.IntN(3) == 0 {
				a, b := writers[rng.IntN(len(writers))], writers[rng.IntN(len(writers))]
				if err := ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, b.StateVector()), nil); err != nil {
					t.Fatal(err)
				}
			}
			snapshots = append(snapshots, EncodeStateAsUpdateV1(w, nil))
		}
		sender := New()
		for _, w := range writers {
			if err := ApplyUpdateV1(sender, EncodeStateAsUpdateV1(w, nil), nil); err != nil {
				t.Fatal(err)
			}
		}
		held := snapshots[rng.IntN(len(snapshots))]
		for _, format := range applyFormats {
			receiver := New(WithMaxPendingItems(math.MaxInt))
			if err := ApplyUpdateV1(receiver, held, nil); err != nil {
				t.Fatal(err)
			}
			if receiver.store.pending != nil || len(receiver.store.pendingDs.clients) > 0 {
				t.Fatalf("round %d: the receiver parks something before the update", round)
			}
			has, all := receiver.StateVector(), sender.StateVector()
			claimed := make(StateVector, len(all))
			// In client order, so the draws, and the update, are the same each run.
			for _, client := range slices.Sorted(maps.Keys(all)) {
				clock := all[client]
				claimed[client] = has.Clock(client)
				if clock > has.Clock(client) {
					claimed[client] += uint64(rng.Int64N(int64(clock - has.Clock(client) + 1)))
				}
			}
			update := EncodeStateAsUpdateV1(sender, claimed)
			if format.v2 {
				var err error
				if update, err = UpdateV1ToV2(update); err != nil {
					t.Fatal(err)
				}
			}
			ds, err := deleteSetOf(newPendingScanner(update, format.v2), format.v2)
			if err != nil {
				t.Fatalf("round %d %s: %v", round, format.name, err)
			}
			budget := newPendingBudget(receiver, receiver.store.StateVector(), update, format.v2)
			clocks, err := structClocksOf(&budget)
			if err != nil {
				t.Fatalf("round %d %s: %v", round, format.name, err)
			}
			predicted := parkedPartsOf(ds, clocks)
			if format.v2 {
				err = ApplyUpdateV2(receiver, update, nil)
			} else {
				err = ApplyUpdateV1(receiver, update, nil)
			}
			if err != nil {
				t.Fatalf("round %d %s: the update was refused: %v", round, format.name, err)
			}
			want := newDeleteSet()
			want.Merge(predicted)
			got := receiver.store.pendingDs
			for _, client := range slices.Sorted(maps.Keys(want.clients)) {
				if !slices.Equal(got.clients[client], want.clients[client]) {
					t.Fatalf("round %d %s: client %d parks %v, checkParkedDeletes named %v",
						round, format.name, client, got.clients[client], want.clients[client])
				}
			}
			for client, ranges := range got.clients {
				if _, named := want.clients[client]; !named && len(ranges) > 0 {
					t.Fatalf("round %d %s: client %d parks %v, checkParkedDeletes named none",
						round, format.name, client, ranges)
				}
			}
			if len(want.clients) > 0 {
				parkedSome++
			}
			// A second update, now that ranges are parked, never leaves more of
			// them than the check counts, and the rest of the document leaves
			// none.
			for _, full := range []bool{false, true} {
				claimed := receiver.StateVector()
				if !full {
					for _, client := range slices.Sorted(maps.Keys(all)) {
						if clock, has := all[client], claimed.Clock(client); clock > has {
							claimed[client] = has + uint64(rng.Int64N(int64(clock-has+1)))
						}
					}
				}
				update := EncodeStateAsUpdateV1(sender, claimed)
				if format.v2 {
					var err error
					if update, err = UpdateV1ToV2(update); err != nil {
						t.Fatal(err)
					}
				}
				ds, err := deleteSetOf(newPendingScanner(update, format.v2), format.v2)
				if err != nil {
					t.Fatalf("round %d %s: %v", round, format.name, err)
				}
				sv := receiver.store.StateVector()
				budget := newPendingBudget(receiver, sv, update, format.v2)
				clocks, err := structClocksOf(&budget)
				if err != nil {
					t.Fatalf("round %d %s: %v", round, format.name, err)
				}
				counted := parkedDeleteRangesAfter(stillParked(receiver.store.pendingDs, sv, clocks), parkedPartsOf(ds, clocks))
				if format.v2 {
					err = ApplyUpdateV2(receiver, update, nil)
				} else {
					err = ApplyUpdateV1(receiver, update, nil)
				}
				if err != nil {
					t.Fatalf("round %d %s: the second update was refused: %v", round, format.name, err)
				}
				got := receiver.PendingStats().DeleteRanges
				if got > counted || (full && counted != 0) {
					t.Fatalf("round %d %s (full %v): %d delete ranges are parked, the check counted %d",
						round, format.name, full, got, counted)
				}
			}
		}
	}
	if parkedSome < rounds/4 {
		t.Fatalf("only %d of %d applies parked a delete range", parkedSome, 2*rounds)
	}
	t.Logf("%d of %d applies parked delete ranges", parkedSome, 2*rounds)
}
