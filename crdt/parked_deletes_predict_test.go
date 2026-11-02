package crdt

import (
	"bytes"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// The ranges checkParkedDeletes predicts must be exactly those the apply parks,
// for random gapped V1 and V2 updates.
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
			ds, err := deleteSetOf(&pendingBudget{update: update, v2: format.v2})
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
			// With ranges already parked, the check must still never
			// undercount.
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
				ds, err := deleteSetOf(&pendingBudget{update: update, v2: format.v2})
				if err != nil {
					t.Fatalf("round %d %s: %v", round, format.name, err)
				}
				sv := receiver.store.StateVector()
				budget := newPendingBudget(receiver, sv, update, format.v2)
				clocks, err := structClocksOf(&budget)
				if err != nil {
					t.Fatalf("round %d %s: %v", round, format.name, err)
				}
				counted := parkedDeleteRangesAfter(receiver.store.pendingDs, receiver.store.pendingDsRanges, sv, clocks, parkedPartsOf(ds, clocks))
				if format.v2 {
					err = ApplyUpdateV2(receiver, update, nil)
				} else {
					err = ApplyUpdateV1(receiver, update, nil)
				}
				if err != nil {
					t.Fatalf("round %d %s: the second update was refused: %v", round, format.name, err)
				}
				got := 0
				for _, ranges := range receiver.store.pendingDs.clients {
					got += len(ranges)
				}
				if got > counted || (full && counted != 0) {
					t.Fatalf("round %d %s (full %v): %d delete ranges are parked, the check counted %d",
						round, format.name, full, got, counted)
				}
				if kept := receiver.PendingStats().DeleteRanges; kept != got {
					t.Fatalf("round %d %s (full %v): PendingStats reports %d parked ranges, the store holds %d",
						round, format.name, full, kept, got)
				}
			}
		}
	}
	if parkedSome < rounds/4 {
		t.Fatalf("only %d of %d applies parked a delete range", parkedSome, 2*rounds)
	}
	t.Logf("%d of %d applies parked delete ranges", parkedSome, 2*rounds)
}

// A whole first pass over a one-client update gives the clocks structClocksOf
// scans for and the delete set walkGroups finds, in V1 and V2.
func TestUnit_PendingBudget_AWholeFirstPassReadsWhatTheStructScansRead(t *testing.T) {
	rng := rand.New(rand.NewPCG(340, 918))
	compared, blocked := 0, 0
	for round := range 200 {
		held, update := gappedUpdate(t, rng)
		receiver := New(WithMaxPendingItems(math.MaxInt))
		if err := ApplyUpdateV1(receiver, held, nil); err != nil {
			t.Fatal(err)
		}
		sv := receiver.store.StateVector()
		for _, target := range []ClientID{1, 2, 3} {
			// The target's structs and the whole delete set.
			others := StateVector{1: 1 << 53, 2: 1 << 53, 3: 1 << 53}
			delete(others, target)
			single, err := DiffUpdateV1(update, others)
			if err != nil {
				t.Fatal(err)
			}
			for _, format := range applyFormats {
				u := single
				if format.v2 {
					if u, err = UpdateV1ToV2(single); err != nil {
						t.Fatal(err)
					}
				}
				whole := newPendingBudget(receiver, sv, u, format.v2)
				whole.setSuperseded(nil)
				if err := whole.readStructs(); err != nil {
					t.Fatalf("round %d %s client %d: %v", round, format.name, target, err)
				}
				scan := newPendingBudget(receiver, sv, u, format.v2)
				want, err := structClocksOf(&scan)
				if err != nil {
					t.Fatalf("round %d %s client %d: %v", round, format.name, target, err)
				}
				if whole.clocks == nil || !maps.Equal(whole.clocks, want) || !bytes.Equal(whole.deleteSetBytes, scan.deleteSetBytes) {
					t.Fatalf("round %d %s client %d: the whole pass reads clocks %v and a %d-byte delete set, the scans %v and %d bytes",
						round, format.name, target, whole.clocks, len(whole.deleteSetBytes), want, len(scan.deleteSetBytes))
				}
				probe := New(WithMaxPendingItems(math.MaxInt))
				if err := ApplyUpdateV1(probe, held, nil); err != nil {
					t.Fatal(err)
				}
				apply := ApplyUpdateV1
				if format.v2 {
					apply = ApplyUpdateV2
				}
				if err := apply(probe, u, nil); err != nil {
					t.Fatalf("round %d %s client %d: %v", round, format.name, target, err)
				}
				if probe.PendingStats().Items > 0 {
					blocked++
				}
				compared++
			}
		}
	}
	if blocked == 0 {
		t.Fatal("no update had a struct the receiver could not integrate")
	}
	t.Logf("%d updates compared, %d with a struct left out", compared, blocked)
}

// The item check and the delete-set read, which read a one-client update's
// structs in one whole pass, refuse each corruption of it as a first pass that
// stops and the group walk after it do, V1 and V2.
func TestUnit_PendingBudget_AWholeFirstPassNamesAFaultAsTheWalkItReplaces(t *testing.T) {
	src := New(WithClientID(4))
	text := src.GetText("t")
	for i := range 30 {
		src.Transact(func(txn *Transaction) { text.Insert(txn, 0, fmt.Sprintf("w%d", i), nil) })
	}
	src.Transact(func(txn *Transaction) {
		for i := range 10 {
			text.Delete(txn, 2*i, 1)
		}
	})
	compared := 0
	for _, f := range formats(t, EncodeStateAsUpdateV1(src, nil)) {
		for at := range f.update {
			for _, value := range []byte{0xc4, 0xff} {
				bad := append([]byte(nil), f.update...)
				bad[at] = value
				budget := func() (*pendingBudget, error) {
					b := newPendingBudget(New(WithMaxPendingItems(4)), StateVector{}, bad, f.name == "V2")
					b.setSuperseded(nil)
					return &b, b.decodeV2()
				}
				wrapped := func(err error) string {
					if err == nil {
						return "<nil>"
					}
					return wrapUpdateErr(err).Error()
				}
				ref, err := budget()
				if err != nil {
					continue // the columns do not read, before any check
				}
				_, want := ref.firstPass(StateVector{}, nil, ref.remaining, false)
				if want == nil {
					want = ref.walkGroups()
				}
				checked, _ := budget()
				got := checked.check()
				if got == nil {
					if err := checked.readStructs(); err != nil {
						got = wrapUpdateErr(err)
					}
				}
				read, _ := budget()
				readErr := read.readStructs()
				walk, _ := budget()
				walkErr := walk.walkGroups()
				if wrapped(want) != wrapped(got) || wrapped(walkErr) != wrapped(readErr) {
					t.Fatalf("%s, byte %d set to %#x: the checks give %v, the pass and the walk %v; the delete-set read gives %v, the walk %v",
						f.name, at, value, got, want, readErr, walkErr)
				}
				compared++
			}
		}
	}
	t.Logf("%d corruptions compared", compared)
}
