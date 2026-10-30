package crdt

import (
	"errors"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
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

// gappedUpdate is a receiver's state, held, and a V1 update that lacks
// structs the receiver needs: it is encoded from a state vector that claims a
// random clock per client, so some structs park and others resolve later in
// the update, in another client's group.
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
// cap has room for before it changes anything, and the same update applies
// on a document with room for what it parks.
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
// it has room for: with room for p the update applies, and with room for p-1
// (p > 1, since a cap of 0 is the default) it is refused and leaves the
// document unchanged.
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
			// A cap that also counts parked delete ranges must have room for them too.
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

// The lenient check refuses exactly what the validating check
// (referencePendingBudgetCheck) refuses, at each room around what the update
// parks.
func TestUnit_PendingBudget_LenientCheckRefusesWhatTheValidatingCheckRefuses(t *testing.T) {
	const rounds = 300
	rng := rand.New(rand.NewPCG(340, 2527))
	compared := 0
	for round := range rounds {
		held, update := gappedUpdate(t, rng)
		for _, format := range applyFormats {
			doc := New(WithMaxPendingItems(math.MaxInt))
			if err := ApplyUpdateV1(doc, held, nil); err != nil {
				t.Fatal(err)
			}
			sv := doc.StateVector()
			if err := applyIn(t, doc, format.v2, update); err != nil {
				t.Fatal(err)
			}
			p := doc.PendingStats().Items
			data := update
			if format.v2 {
				var err error
				if data, err = UpdateV1ToV2(update); err != nil {
					t.Fatal(err)
				}
			}
			var v2 *v2Decoder
			var rest encoding.Decoder
			if format.v2 {
				d, err := newV2Decoder(data)
				if err != nil {
					t.Fatal(err)
				}
				v2, rest = d, *d.restDec
			}
			superseded, _, err := supersededGroups(data, v2, rest)
			if err != nil {
				t.Fatal(err)
			}
			for _, room := range []int{0, 1, p - 1, p, p + 1, len(data)} {
				if room < 0 {
					continue
				}
				// The check finds the superseded groups and the struct count
				// itself, so its first pass may stop early; given the groups
				// without the count, it stops early only in the last group.
				for _, b := range []pendingBudget{
					{initial: sv, update: data, remaining: room, v2: format.v2},
					{initial: sv, update: data, remaining: room, v2: format.v2, superseded: superseded, groupsKnown: true},
				} {
					want := referencePendingBudgetCheck(b)
					got := b.check()
					if (got == nil) != (want == nil) {
						t.Fatalf("round %d %s room %d groups known %v (the update parks %d): the check returned %v, the validating check %v",
							round, format.name, room, b.groupsKnown, p, got, want)
					}
					compared++
				}
			}
		}
	}
	t.Logf("%d checks agreed", compared)
}

// withStringPool is the V2 update with its string column's pool replaced by
// pool, the column's lengths kept.
func withStringPool(t *testing.T, update []byte, pool func(string) string) []byte {
	t.Helper()
	dec := encoding.NewDecoder(update)
	enc := encoding.NewEncoder()
	flag, err := dec.ReadVarUint()
	if err != nil {
		t.Fatal(err)
	}
	enc.WriteVarUint(flag)
	for column := range 9 {
		b, err := dec.ReadVarBytes()
		if err != nil {
			t.Fatal(err)
		}
		if column == 5 { // the string column: the pool, then its lengths
			old := encoding.NewDecoder(b)
			s, err := old.ReadVarString()
			if err != nil {
				t.Fatal(err)
			}
			col := encoding.NewEncoder()
			col.WriteVarString(pool(s))
			b = append(col.Bytes(), old.RemainingBytes()...)
		}
		enc.WriteVarBytes(b)
	}
	return append(enc.Bytes(), dec.RemainingBytes()...)
}

// The check counts a V2 string's clocks as the apply decodes it, not as the
// lengths column claims, so an update whose pool is shorter than its lengths
// is refused before it changes anything.
func TestUnit_PendingBudget_V2StringClocksFollowThePoolNotTheLengths(t *testing.T) {
	const limit, parked = 2, 5
	author := New(WithClientID(1))
	authorText := author.GetText("t")
	author.Transact(func(txn *Transaction) { authorText.Insert(txn, 0, "abcdefghij", nil) })
	editor := New(WithClientID(2))
	if err := ApplyUpdateV1(editor, EncodeStateAsUpdateV1(author, nil), nil); err != nil {
		t.Fatal(err)
	}
	editorText := editor.GetText("t")
	for range parked {
		editor.Transact(func(txn *Transaction) { editorText.Insert(txn, 10, "x", nil) })
	}
	update := withStringPool(t, EncodeStateAsUpdateV2(editor, nil), func(s string) string {
		if !strings.HasSuffix(s, "abcdefghij") {
			t.Fatalf("the pool %q does not end with the author's text", s)
		}
		return strings.TrimSuffix(s, "cdefghij")
	})

	doc := New(WithMaxPendingItems(limit))
	before, text := EncodeStateAsUpdateV1(doc, nil), doc.GetText("t").ToString()
	updates := observed(doc)
	if err := ApplyUpdateV2(doc, update, nil); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("the update was applied with err %v, want ErrInvalidUpdate", err)
	}
	requireUnchanged(t, doc, before, text, updates)

	roomy := New(WithMaxPendingItems(parked))
	if err := ApplyUpdateV2(roomy, update, nil); err != nil {
		t.Fatalf("with room for its %d items the update was refused: %v", parked, err)
	}
	if got := roomy.GetText("t").ToString(); got != "ab" {
		t.Fatalf("the text is %q, want %q", got, "ab")
	}
	if got := roomy.PendingStats().Items; got != parked {
		t.Fatalf("%d items are parked, want %d", got, parked)
	}
}

// An update that fails before the apply changes anything tells no observer,
// as one the pending cap refuses does not.
func TestUnit_ApplyUpdate_FailureBeforeAnyChangeTellsNoObserver(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		update  []byte
	}{
		{"V1 truncated header", 1, []byte{0xff}},
		{"V1 more groups than bytes", 1, []byte{100, 0}},
		{"V1 malformed later group", 1, repeatedGroupsUpdate(t, 1, true)},
		{"V2 truncated header", 2, []byte{0xff}},
		{"V2 malformed later group", 2, repeatedGroupsUpdate(t, 2, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := New()
			before, text := EncodeStateAsUpdateV1(doc, nil), doc.GetText("t").ToString()
			updates := observed(doc)
			txns := 0
			doc.OnAfterTransaction(func(*Transaction) { txns++ })
			if err := applyVersion(doc, tc.version, tc.update); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("err %v, want ErrInvalidUpdate", err)
			}
			requireUnchanged(t, doc, before, text, updates)
			if txns != 0 {
				t.Fatalf("OnAfterTransaction fired %d times, want none", txns)
			}
		})
	}
}
