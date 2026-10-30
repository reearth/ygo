package crdt

import (
	"errors"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
)

// applyIn applies the V1 update to doc as V1, or converted to V2.
func applyIn(t *testing.T, doc *Doc, v2 bool, update []byte) error {
	t.Helper()
	if !v2 {
		return ApplyUpdateV1(doc, update, nil)
	}
	converted, err := UpdateV1ToV2(update)
	if err != nil {
		t.Fatal(err)
	}
	return ApplyUpdateV2(doc, converted, nil)
}

var applyFormats = []struct {
	name string
	v2   bool
}{{"V1", false}, {"V2", true}}

// observed counts the updates doc's OnUpdate observers are handed.
func observed(doc *Doc) *int {
	n := new(int)
	doc.OnUpdate(func([]byte, any) { *n++ })
	return n
}

// requireUnchanged requires doc to encode as before did and its text to read as
// text did, and its observers to have been handed nothing since updates was
// zero.
func requireUnchanged(t *testing.T, doc *Doc, before []byte, text string, updates *int) {
	t.Helper()
	if after := EncodeStateAsUpdateV1(doc, nil); string(after) != string(before) {
		t.Fatalf("the document's state changed: %d bytes before, %d after", len(before), len(after))
	}
	if got := doc.GetText("t").ToString(); got != text {
		t.Fatalf("the text changed from %q to %q", text, got)
	}
	if *updates != 0 {
		t.Fatalf("the observers were handed %d updates, want none", *updates)
	}
}

// parkingEdit is an update of client 2 that inserts text into "t" and sets n
// keys on a map client 1 created, which the receiver never got, so every
// entry parks waiting for its parent while the text integrates. author is the
// state the receiver lacks.
func parkingEdit(t *testing.T, n int) (update, author []byte) {
	t.Helper()
	creator := New(WithClientID(1))
	root := creator.GetMap("m")
	creator.Transact(func(txn *Transaction) { root.Set(txn, "nested", NewMapPrelim()) })
	author = EncodeStateAsUpdateV1(creator, nil)
	editor := New(WithClientID(2))
	if err := ApplyUpdateV1(editor, author, nil); err != nil {
		t.Fatal(err)
	}
	v, _ := editor.GetMap("m").Get("nested")
	nested, ok := v.(*YMap)
	if !ok {
		t.Fatalf("nested is %T", v)
	}
	text := editor.GetText("t")
	editor.Transact(func(txn *Transaction) {
		text.Insert(txn, 0, "hello", nil)
		for i := range n {
			nested.Set(txn, "k"+strconv.Itoa(i), i)
		}
	})
	return EncodeStateAsUpdateV1(editor, creator.StateVector()), author
}

// gappedUpdate is a receiver's state, held, and an update, both V1, that lacks
// structs the receiver needs. Three writers edit a text, a map and an array
// nested in it, syncing at random; held is a state one writer had, and update
// is the whole document encoded from a state vector that claims, for each
// client, a random clock between what held holds and what the document holds.
// Structs after a gap, and items whose origin, right origin or parent lies in
// it, park, while structs whose dependencies come later in the update, in
// another client's group, integrate.
func gappedUpdate(t *testing.T, rng *rand.Rand) (held, update []byte) {
	t.Helper()
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
	held = snapshots[rng.IntN(len(snapshots))]
	receiver := New()
	if err := ApplyUpdateV1(receiver, held, nil); err != nil {
		t.Fatal(err)
	}
	has, all := receiver.StateVector(), sender.StateVector()
	claimed := make(StateVector, len(all))
	// In client order, so that a seed draws the same clocks on every run.
	for _, client := range slices.Sorted(maps.Keys(all)) {
		clock := all[client]
		claimed[client] = has.Clock(client)
		if clock > has.Clock(client) {
			claimed[client] += uint64(rng.Int64N(int64(clock - has.Clock(client) + 1)))
		}
	}
	return held, EncodeStateAsUpdateV1(sender, claimed)
}

// A document refuses an update that would park more items than the pending
// cap (WithMaxPendingItems) has room for before it changes anything: an
// update that inserts text and parks five map entries under a parent the
// document lacks leaves a document capped at two as it was, with nothing
// parked and no observer told. Before, the refusal came after the text had
// integrated: the document kept the text, fired OnUpdate, and returned
// ErrInvalidUpdate. The same edit applies on a document whose cap has room
// for what it parks, and its entries integrate once their parent arrives.
func TestUnit_ApplyUpdate_RefusedParkedItemUpdateLeavesTheDocumentUnchanged(t *testing.T) {
	const limit, parked = 2, 5
	update, author := parkingEdit(t, parked)
	for _, format := range applyFormats {
		t.Run(format.name, func(t *testing.T) {
			doc := New(WithMaxPendingItems(limit))
			before, text := EncodeStateAsUpdateV1(doc, nil), doc.GetText("t").ToString()
			updates := observed(doc)
			if err := applyIn(t, doc, format.v2, update); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("the update was applied with err %v, want ErrInvalidUpdate", err)
			}
			requireUnchanged(t, doc, before, text, updates)
			if got := doc.PendingStats().Items; got != 0 {
				t.Fatalf("after the refusal %d items are parked, want none", got)
			}

			roomy := New(WithMaxPendingItems(parked))
			if err := applyIn(t, roomy, format.v2, update); err != nil {
				t.Fatalf("with room for its %d items the update was refused: %v", parked, err)
			}
			if got := roomy.GetText("t").ToString(); got != "hello" {
				t.Fatalf("the text is %q, want %q", got, "hello")
			}
			if got := roomy.PendingStats().Items; got != parked {
				t.Fatalf("%d items are parked, want %d", got, parked)
			}
			if err := ApplyUpdateV1(roomy, author, nil); err != nil {
				t.Fatal(err)
			}
			if got := roomy.PendingStats().Items; got != 0 {
				t.Fatalf("once the parent arrived %d items are still parked", got)
			}
		})
	}
}

// The pending cap refuses exactly the updates that would park more items than
// it has room for, before they change anything. For random updates with gaps
// and cross-client dependencies (gappedUpdate), an update that parks p items
// applies on a document whose cap leaves room for p (and for the delete
// ranges it parks, should the cap count them), and is refused on one whose
// cap leaves room for p-1, which it leaves unchanged. The second is tried
// where the update parks fewer than p delete ranges, so only its items can
// pass the cap, and p is more than one, since a cap of zero is the default.
func TestUnit_ApplyUpdate_ParkedItemCapRefusesExactlyWhatWouldPassIt(t *testing.T) {
	const rounds = 300
	rng := rand.New(rand.NewPCG(338, 2526))
	refused := 0
	for round := range rounds {
		held, update := gappedUpdate(t, rng)
		for _, format := range applyFormats {
			load := func(limit int) *Doc {
				doc := New(WithMaxPendingItems(limit))
				if err := ApplyUpdateV1(doc, held, nil); err != nil {
					t.Fatal(err)
				}
				return doc
			}
			free := load(math.MaxInt)
			if err := applyIn(t, free, format.v2, update); err != nil {
				t.Fatalf("round %d %s: with the cap lifted the update was refused: %v", round, format.name, err)
			}
			p, d := free.PendingStats().Items, free.PendingStats().DeleteRanges
			if p == 0 {
				continue
			}
			fits := load(max(p, d))
			if err := applyIn(t, fits, format.v2, update); err != nil {
				t.Fatalf("round %d %s: an update parking %d items and %d delete ranges was refused with room for %d: %v",
					round, format.name, p, d, max(p, d), err)
			}
			if string(EncodeStateAsUpdateV1(fits, nil)) != string(EncodeStateAsUpdateV1(free, nil)) {
				t.Fatalf("round %d %s: with room for its items the update applied differently", round, format.name)
			}
			if p == 1 || d >= p {
				continue
			}
			over := load(p - 1)
			before := EncodeStateAsUpdateV1(over, nil)
			updates := observed(over)
			if err := applyIn(t, over, format.v2, update); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("round %d %s: an update parking %d items applied with room for %d (err %v)", round, format.name, p, p-1, err)
			}
			if string(EncodeStateAsUpdateV1(over, nil)) != string(before) || *updates != 0 {
				t.Fatalf("round %d %s: the refused update changed the document or told its observers", round, format.name)
			}
			refused++
		}
	}
	if refused < rounds/4 {
		t.Fatalf("only %d of %d applies were refused a cap one short of the items they park", refused, 2*rounds)
	}
	t.Logf("%d of %d applies were refused a cap one short of the items they park", refused, 2*rounds)
}
