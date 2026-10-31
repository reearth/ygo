package crdt

import (
	"bytes"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// walkedUpdate is a random V1 update of every content kind, starting mid-run
// and sometimes holding skips, with a receiver's state vector.
func walkedUpdate(t testing.TB, rng *rand.Rand) (update []byte, held StateVector) {
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
		// Merging two updates of one writer around a gap yields a skip.
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

// The group walk must reach the delete set the full scan reads, and
// parkingRangesBound must never undercount.
func TestUnit_WalkGroups_TheWalkReadsWhatTheFullScanReads(t *testing.T) {
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
			var wantDS DeleteSet
			if format.v2 {
				wantDS, err = decodeDeleteSetV2(full.v2)
			} else {
				wantDS, err = decodeDeleteSet(full.rest)
			}
			if err != nil {
				t.Fatalf("round %d %s: the full scan's delete set does not read: %v", round, format.name, err)
			}

			b := pendingBudget{update: update, v2: format.v2}
			gotDS, err := deleteSetOf(&b)
			if err != nil {
				t.Fatalf("round %d %s: deleteSetOf: %v", round, format.name, err)
			}
			if !reflect.DeepEqual(gotDS, wantDS) {
				t.Fatalf("round %d %s: after the walk the delete set is %v, after the full scan %v", round, format.name, gotDS, wantDS)
			}
			bound, err := parkingRangesBound(b.deleteSetScanner(), held)
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
			if format.v2 {
				// The delete set lies on the rest cursor, two bytes a range at
				// least, which checkParkedDeletes's skip relies on.
				dec, err := newV2Decoder(update)
				if err != nil {
					t.Fatal(err)
				}
				start := *dec
				applied := pendingBudget{update: update, v2: true, v2Start: &start, v2Rest: *dec.restDec}
				ranges, err := parkingRangesBound(b.deleteSetScanner(), StateVector{})
				if err != nil {
					t.Fatal(err)
				}
				if limit := applied.deleteSetLimit(); 2*ranges > limit || len(b.deleteSetBytes) > limit {
					t.Fatalf("round %d: %d ranges in %d delete-set bytes, past the rest cursor's %d bytes", round, ranges, len(b.deleteSetBytes), limit)
				}
			}
		}
	}
	if skips == 0 {
		t.Fatal("no update held a skip")
	}
}

// separateWalks is what the two walks that walkGroups replaced read of an
// update: the client of each group, the struct count and the bytes past the
// structs.
type separateWalks struct {
	ok             bool
	clients        []ClientID
	structs        uint64
	deleteSetBytes []byte
}

// walkSeparately runs the two walks the apply ran before walkGroups: the
// group scan of knowSuperseded, on copies of the column cursors with no rest
// cursor bound, and the struct walk of checkParkedDeletes, on a fresh scanner.
func walkSeparately(t testing.TB, update []byte, v2 bool) separateWalks {
	t.Helper()
	frame := func(s *pendingScanner) (w separateWalks) {
		s.frame = true
		n := s.uint()
		if n > maxV2Items {
			return separateWalks{}
		}
		for i := uint64(0); i < n && s.err == nil; i++ {
			items := s.uint()
			w.structs += items
			if w.structs > maxV2Items {
				return separateWalks{}
			}
			w.clients = append(w.clients, s.client())
			s.uint()
			for j := uint64(0); j < items && s.err == nil; j++ {
				s.item()
			}
		}
		if s.err != nil {
			return separateWalks{}
		}
		w.ok, w.deleteSetBytes = true, s.rest.RemainingBytes()
		return w
	}
	var groupScan separateWalks
	if !v2 {
		groupScan = frame(&pendingScanner{rest: encoding.NewDecoder(update)})
	} else if dec, err := newV2Decoder(update); err == nil {
		rest := *dec.restDec
		dec.restDec = nil
		groupScan = frame(&pendingScanner{v2: dec, rest: &rest})
	}
	structWalk := newPendingScanner(update, v2)
	if structWalk.err != nil {
		return separateWalks{}
	}
	walked := frame(structWalk)
	if groupScan.ok != walked.ok || !slices.Equal(groupScan.clients, walked.clients) || groupScan.structs != walked.structs ||
		!bytes.Equal(groupScan.deleteSetBytes, walked.deleteSetBytes) {
		t.Fatalf("the two separate walks disagree on update %x", update)
	}
	return walked
}

// checkWalkGroups fails t unless walkGroups, from a fresh budget and from one
// set up as applyV2Txn sets it up, finds the superseded groups and delete-set
// position the separate walks find, and its scan counts their groups.
func checkWalkGroups(t testing.TB, update []byte, v2 bool) (ok bool) {
	t.Helper()
	want := walkSeparately(t, update, v2)
	budgets := []pendingBudget{{update: update, v2: v2}}
	if dec, err := newV2Decoder(update); v2 && err == nil {
		start := *dec
		budgets = append(budgets, pendingBudget{update: update, v2: true, v2Start: &start, v2Rest: *dec.restDec})
	}
	for i, b := range budgets {
		err := b.walkGroups()
		if (err == nil) != want.ok {
			t.Fatalf("budget %d V2=%v: walkGroups error %v, separate walks read the update: %v (update %x)", i, v2, err, want.ok, update)
		}
		if err != nil {
			continue
		}
		// Every range that can park, two bytes at least, lies in deleteSetLimit.
		if n, err := parkingRangesBound(b.deleteSetScanner(), StateVector{}); err == nil && 2*n > b.deleteSetLimit() {
			t.Fatalf("budget %d V2=%v: %d ranges, past the %d bytes deleteSetLimit allows (update %x)", i, v2, n, b.deleteSetLimit(), update)
		}
		last := make(map[ClientID]int, len(want.clients))
		for g, client := range want.clients {
			last[client] = g
		}
		for g, client := range want.clients {
			if superseded := last[client] != g; b.superseded.has(uint64(g)) != superseded {
				t.Fatalf("budget %d V2=%v: group %d (client %d) superseded %v, separate walks %v (update %x)", i, v2, g, client, b.superseded.has(uint64(g)), superseded, update)
			}
		}
		var rest encoding.Decoder
		var start *v2Decoder
		if b.v2Start != nil {
			start, rest = b.v2Start, b.v2Rest
		} else if v2 {
			dec, _ := newV2Decoder(update)
			start, rest = dec, *dec.restDec
		}
		_, groups, _, _ := scanGroupClients(update, start, rest, nil)
		if groups != uint64(len(want.clients)) {
			t.Fatalf("budget %d V2=%v: the walk counts %d groups, separate walks %d", i, v2, groups, len(want.clients))
		}
		if !bytes.Equal(b.deleteSetBytes, want.deleteSetBytes) {
			t.Fatalf("budget %d V2=%v: the delete set starts %d bytes from the end, separate walks %d (update %x)", i, v2, len(b.deleteSetBytes), len(want.deleteSetBytes), update)
		}
		sv := StateVector{}
		for _, client := range want.clients {
			sv[client] = uint64(client % 7)
		}
		got, gotErr := parkingRangesBound(b.deleteSetScanner(), sv)
		wantScanner := pendingScanner{rest: encoding.NewDecoder(want.deleteSetBytes)}
		if v2 {
			wantScanner.v2 = &v2Decoder{restDec: wantScanner.rest}
		}
		bound, boundErr := parkingRangesBound(&wantScanner, sv)
		if got != bound || (gotErr == nil) != (boundErr == nil) {
			t.Fatalf("budget %d V2=%v: parkingRangesBound %d, %v on the shared walk, %d, %v on the separate walks", i, v2, got, gotErr, bound, boundErr)
		}
	}
	return want.ok
}

// mutate is update with one random byte-level change.
func mutate(rng *rand.Rand, update []byte) []byte {
	out := slices.Clone(update)
	if len(out) == 0 {
		return []byte{byte(rng.IntN(256))}
	}
	i := rng.IntN(len(out))
	switch rng.IntN(5) {
	case 0:
		out[i] ^= 1 << rng.IntN(8)
	case 1:
		out[i] = byte(rng.IntN(256))
	case 2:
		out = out[:i]
	case 3:
		out = slices.Insert(out, i, byte(rng.IntN(256)))
	default:
		out = slices.Delete(out, i, i+1)
	}
	return out
}

// walkCorpus is valid V1 and V2 updates: documents of every content kind, and
// groups in ascending, descending, repeated and shuffled client order, each
// with a delete set.
func walkCorpus(t testing.TB, rng *rand.Rand) (updates [][]byte, v2 []bool) {
	t.Helper()
	add := func(update []byte, isV2 bool) {
		updates, v2 = append(updates, update), append(v2, isV2)
	}
	for range 40 {
		v1, _ := walkedUpdate(t, rng)
		converted, err := UpdateV1ToV2(v1)
		if err != nil {
			t.Fatal(err)
		}
		add(v1, false)
		add(converted, true)
	}
	source, subdoc := New(), New()
	t.Cleanup(source.Destroy)
	t.Cleanup(subdoc.Destroy)
	contents := []Content{
		NewContentString("А🐷z"), NewContentDeleted(3), NewContentJSON("a", nil, 7), NewContentBinary([]byte{1, 2}),
		NewContentEmbed(map[string]any{"n": []any{1, true}}), NewContentFormat("bold", true),
		NewContentType(&NewMapPrelim().abstractType), NewContentType(&NewYXmlElement("div").abstractType),
		NewContentAny([]any{1, "a"}, map[string]any{"key": 2}), NewContentDoc(subdoc), NewContentMove(&ID{Client: 9}, 2),
	}
	parent := ID{Client: 999}
	for _, order := range []string{"ascending", "descending", "repeats", "shuffled"} {
		for range 10 {
			clients := make([]ClientID, 0, 12)
			for i := range 1 + rng.IntN(12) {
				switch order {
				case "ascending":
					clients = append(clients, ClientID(i+1))
				case "descending":
					clients = append(clients, ClientID(12-i))
				default:
					clients = append(clients, ClientID(1+rng.IntN(4)))
				}
			}
			if order == "shuffled" {
				rng.Shuffle(len(clients), func(i, j int) { clients[i], clients[j] = clients[j], clients[i] })
			}
			groups := make([][]*Item, len(clients))
			skips := map[int]uint64{}
			for i, c := range clients {
				var clock uint64
				for j := range 1 + rng.IntN(3) {
					key := fmt.Sprint("k", (i+j)%5)
					content := contents[rng.IntN(len(contents))]
					groups[i] = append(groups[i], &Item{ID: ID{Client: c, Clock: clock}, parentID: &parent, ParentSub: &key, Content: content})
					clock += uint64(content.Len())
				}
				if rng.IntN(4) == 0 {
					skips[i] = 1 + uint64(rng.IntN(5))
				}
			}
			ds := newDeleteSet()
			for range rng.IntN(6) {
				ds.add(ID{Client: ClientID(1 + rng.IntN(6)), Clock: uint64(rng.IntN(40))}, 1+rng.IntN(5))
			}
			for _, version := range []int{1, 2} {
				add(groupsUpdate(source, groups, skips, ds, version), version == 2)
			}
		}
	}
	return updates, v2
}

// Differential fuzz: on valid updates and on byte-level mutations of them, the
// one group walk finds what the two separate walks found.
func TestUnit_WalkGroups_MatchesTheSeparateWalks(t *testing.T) {
	rng := rand.New(rand.NewPCG(334, 1))
	updates, v2 := walkCorpus(t, rng)
	read, unordered := 0, 0
	for i, update := range updates {
		if ordered, _, _, err := scanGroupClients(update, nil, encoding.Decoder{}, nil); !v2[i] && err == nil && !ordered {
			unordered++
		}
		if !checkWalkGroups(t, update, v2[i]) {
			t.Fatalf("valid update %d (V2=%v) does not walk", i, v2[i])
		}
		for range 60 {
			mutated := update
			for range 1 + rng.IntN(3) {
				mutated = mutate(rng, mutated)
			}
			if checkWalkGroups(t, mutated, v2[i]) {
				read++
			}
		}
	}
	if unordered == 0 || read == 0 {
		t.Fatalf("the corpus covers %d unordered V1 updates and %d mutations that still walk", unordered, read)
	}
	t.Logf("%d updates, %d unordered V1, %d of %d mutations still walk", len(updates), unordered, read, 60*len(updates))
}

// FuzzWalkGroups runs the comparison of TestUnit_WalkGroups_MatchesTheSeparateWalks
// on inputs go test -fuzz derives from the same corpus, read as V1 and as V2.
func FuzzWalkGroups(f *testing.F) {
	rng := rand.New(rand.NewPCG(334, 2))
	updates, _ := walkCorpus(f, rng)
	for _, update := range updates {
		f.Add(update)
	}
	f.Fuzz(func(t *testing.T, update []byte) {
		checkWalkGroups(t, update, false)
		checkWalkGroups(t, update, true)
	})
}

// A V2 update's strings are on its columns, not its rest cursor, so a long
// one whose delete set fits the room skips the walk, while its V1 form, which
// holds the strings inline, walks.
func TestUnit_CheckParkedDeletes_V2SkipsTheWalkWhenItsRestCursorFits(t *testing.T) {
	author := New(WithClientID(1))
	text := author.GetText("t")
	author.Transact(func(txn *Transaction) { text.Insert(txn, 0, strings.Repeat("abcdefgh", 2_000), nil) })
	for _, at := range []int{9, 7, 5, 3} {
		author.Transact(func(txn *Transaction) { text.Delete(txn, at, 1) })
	}
	v1 := EncodeStateAsUpdateV1(author, nil)
	v2 := EncodeStateAsUpdateV2(author, nil)
	dec, err := newV2Decoder(v2)
	if err != nil {
		t.Fatal(err)
	}
	rest := dec.restDec.Remaining()
	limit := rest / 2
	if len(v2)/2 <= limit || len(v1)/2 <= limit {
		t.Fatalf("the update (V1 %d, V2 %d bytes, rest %d) does not reach past the room", len(v1), len(v2), rest)
	}
	for _, format := range applyFormats {
		doc := New(WithMaxPendingItems(limit))
		update := v1
		b := newPendingBudget(doc, doc.store.StateVector(), update, false)
		if format.v2 {
			update = v2
			d, err := newV2Decoder(update)
			if err != nil {
				t.Fatal(err)
			}
			start := *d
			b = newPendingBudget(doc, doc.store.StateVector(), update, true)
			b.v2Start, b.v2Rest = &start, *d.restDec
		}
		if err := checkParkedDeletes(doc, &b); err != nil {
			t.Fatalf("%s: checkParkedDeletes: %v", format.name, err)
		}
		if b.walked != !format.v2 {
			t.Fatalf("%s: the check walked the update: %v, want %v", format.name, b.walked, !format.v2)
		}
	}
}
