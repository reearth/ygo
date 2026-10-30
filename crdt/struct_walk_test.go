package crdt

import (
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

// walkedUpdate is a V1 update of a document three writers edit at random, with
// text, formats, embeds, map values of every kind, subdocuments and nested
// arrays, and deletes, and a state vector a receiver could hold. The update
// is encoded from a random clock per client, so its structs start mid-run; one
// update in three is the merge of two updates with a gap between them, so it
// holds skips.
func walkedUpdate(t *testing.T, rng *rand.Rand) (update []byte, held StateVector) {
	t.Helper()
	writers := []*Doc{New(WithClientID(1)), New(WithClientID(2)), New(WithClientID(3))}
	for range 30 {
		w := writers[rng.IntN(len(writers))]
		text, root, arr := w.GetText("t"), w.GetMap("m"), w.GetArray("a")
		n, l := text.Len(), arr.Len()
		w.Transact(func(txn *Transaction) {
			switch op := rng.IntN(9); {
			case op == 0 && n > 0:
				pos := rng.IntN(n)
				text.Delete(txn, pos, 1+rng.IntN(min(3, n-pos)))
			case op == 1 && n > 1:
				text.Format(txn, 0, 1+rng.IntN(n-1), map[string]any{"bold": true, "link": map[string]any{"href": "a"}})
			case op == 2:
				text.InsertEmbed(txn, rng.IntN(n+1), map[string]any{"image": "x.png"}, nil)
			case op == 3:
				values := []any{rng.IntN(100), "two", []byte{3}, map[string]any{"four": 4.5}, nil, true}
				root.Set(txn, string(rune('a'+rng.IntN(4))), values[rng.IntN(len(values))])
			case op == 4:
				root.Set(txn, "sub", New())
			case op == 5:
				root.Set(txn, "nested", NewMapPrelim())
			case op == 6 && l > 0:
				arr.Delete(txn, rng.IntN(l), 1)
			case op == 6:
				arr.Insert(txn, 0, []any{rng.IntN(10), "x", []byte{1}})
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
	}
	sender := New()
	for _, w := range writers {
		if err := ApplyUpdateV1(sender, EncodeStateAsUpdateV1(w, nil), nil); err != nil {
			t.Fatal(err)
		}
	}
	all := sender.StateVector()
	held = make(StateVector, len(all))
	for _, client := range slices.Sorted(maps.Keys(all)) {
		held[client] = uint64(rng.Int64N(int64(all[client] + 1)))
	}
	update = EncodeStateAsUpdateV1(sender, held)
	if rng.IntN(3) == 0 {
		// Two updates of one writer with a gap between them merge into one
		// that holds a skip over the gap.
		w := writers[rng.IntN(len(writers))]
		text := w.GetText("t")
		// update ends where w stood; "gap" is left out of the merge.
		w.Transact(func(txn *Transaction) { text.Insert(txn, 0, "gap", nil) })
		mid := w.StateVector()
		w.Transact(func(txn *Transaction) { text.Insert(txn, 0, "after", nil) })
		merged, err := MergeUpdatesV1(update, EncodeStateAsUpdateV1(w, mid))
		if err != nil {
			t.Fatal(err)
		}
		update = merged
	}
	return update, held
}

// The framing walk must leave the scan at the delete set the full scan reads,
// and parkingRangesBound must count no fewer ranges than park.
func TestUnit_FrameStructs_TheWalkReadsWhatTheFullScanReads(t *testing.T) {
	rng := rand.New(rand.NewPCG(334, 340))
	skips := 0
	for round := range 500 {
		v1, held := walkedUpdate(t, rng)
		v2, err := UpdateV1ToV2(v1)
		if err != nil {
			t.Fatal(err)
		}
		for _, format := range applyFormats {
			update := v1
			if format.v2 {
				update = v2
			}
			readDS := func(s *pendingScanner) DeleteSet {
				t.Helper()
				var ds DeleteSet
				var err error
				if format.v2 {
					ds, err = decodeDeleteSetV2(s.v2)
				} else {
					ds, err = decodeDeleteSet(s.rest)
				}
				if err != nil || s.err != nil {
					t.Fatalf("round %d %s: the delete set does not read: %v %v", round, format.name, err, s.err)
				}
				return ds
			}
			full := newPendingScanner(update, format.v2)
			for range full.uint() {
				n := full.uint()
				full.client()
				full.uint()
				for range n {
					if _, skip, _, _ := full.item(); skip {
						skips++
					}
				}
			}
			if full.err != nil {
				t.Fatalf("round %d %s: the full scan fails: %v", round, format.name, full.err)
			}
			wantDS := readDS(full)

			walk := newPendingScanner(update, format.v2)
			if err := frameStructs(walk); err != nil {
				t.Fatalf("round %d %s: frameStructs: %v", round, format.name, err)
			}
			if gotDS := readDS(walk); !reflect.DeepEqual(gotDS, wantDS) {
				t.Fatalf("round %d %s: after the walk the delete set is %v, after the full scan %v", round, format.name, gotDS, wantDS)
			}

			bounded := newPendingScanner(update, format.v2)
			if err := frameStructs(bounded); err != nil {
				t.Fatal(err)
			}
			bound, err := parkingRangesBound(bounded, held)
			if err != nil {
				t.Fatalf("round %d %s: parkingRangesBound: %v", round, format.name, err)
			}
			exact := 0
			for _, parts := range parkedPartsOf(wantDS, held).clients {
				exact += len(parts)
			}
			if bound < exact {
				t.Fatalf("round %d %s: parkingRangesBound = %d, below the %d ranges that park", round, format.name, bound, exact)
			}
		}
	}
	if skips == 0 {
		t.Fatal("no update held a skip")
	}
}
