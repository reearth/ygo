package crdt

import (
	"math/rand/v2"
	"testing"
)

// The pending cap's check before the apply costs a small update nothing: for a
// keystroke, in V1 (a comparison of its length with the room left) and in V2
// (a count of its structs from its group headers), it allocates nothing, and
// it leaves the V2 decoder where the apply begins to read.
func TestUnit_PendingBudget_CheckBeforeApplyAllocatesNothingForAKeystroke(t *testing.T) {
	base := New(WithClientID(1))
	text := base.GetText("t")
	base.Transact(func(txn *Transaction) { text.Insert(txn, 0, "hello world", nil) })
	state := EncodeStateAsUpdateV1(base, nil)
	sv := base.StateVector()
	base.Transact(func(txn *Transaction) { text.Insert(txn, 3, "x", nil) })
	keystroke := EncodeStateAsUpdateV1(base, sv)
	keystrokeV2, err := UpdateV1ToV2(keystroke)
	if err != nil {
		t.Fatal(err)
	}
	doc := New()
	if err := ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatal(err)
	}
	docSV := doc.StateVector()

	v1 := newPendingBudget(doc, docSV, keystroke, false)
	if n := testing.AllocsPerRun(100, func() {
		if err := v1.checkBeforeApply(len(keystroke)); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("the V1 check allocated %v times", n)
	}

	dec, err := newV2Decoder(keystrokeV2)
	if err != nil {
		t.Fatal(err)
	}
	initial := *dec
	v2 := newPendingBudget(doc, docSV, keystrokeV2, true)
	v2.v2Start, v2.v2Rest = &initial, *dec.restDec
	if n := testing.AllocsPerRun(100, func() {
		if err := v2.checkBeforeApply(len(keystrokeV2)); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("the V2 check allocated %v times", n)
	}
	if start := v2.v2Rest; len(dec.restDec.RemainingBytes()) != len(start.RemainingBytes()) {
		t.Fatal("the V2 check moved the apply's decoder")
	}
	if err := ApplyUpdateV2(doc, keystrokeV2, nil); err != nil {
		t.Fatal(err)
	}
	if got := doc.GetText("t").ToString(); got != "helxlo world" {
		t.Fatalf("the text is %q", got)
	}
}

// v2StructCount, the bound on a V2 update's structs read from its group
// headers, is the number of structs the full scan of the update reads, skips
// included, for updates of several groups holding text, map values, nested
// arrays, deletes and skips (gappedUpdate), and for every kind of content
// whose value is on the rest cursor (binary, embeds, formats, Any values,
// subdocuments) in a group before the last.
func TestUnit_PendingBudget_V2StructCountIsTheScannedCount(t *testing.T) {
	count := func(t *testing.T, v1 []byte) {
		t.Helper()
		v2, err := UpdateV1ToV2(v1)
		if err != nil {
			t.Fatal(err)
		}
		s := newPendingScanner(v2, true)
		want := 0
		clients := s.uint()
		for range clients {
			n := s.uint()
			want += int(n)
			s.client()
			s.uint()
			for range n {
				s.item()
			}
		}
		if s.err != nil {
			t.Fatal(s.err)
		}
		d, err := newV2Decoder(v2)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := v2StructCount(d); !ok || got != want {
			t.Fatalf("v2StructCount = %d, %v; the scan reads %d structs", got, ok, want)
		}
	}
	rng := rand.New(rand.NewPCG(340, 2526))
	for range 300 {
		_, update := gappedUpdate(t, rng)
		count(t, update)
	}

	// V2 writes client groups from the highest client down, so client 2's
	// group, with every kind of rest content, comes before client 1's, and the
	// walk steps over all of it to find client 1's header.
	doc := New(WithClientID(2))
	text, arr, m := doc.GetText("t"), doc.GetArray("a"), doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		text.Insert(txn, 0, "hello", nil)
		text.InsertEmbed(txn, 5, map[string]any{"image": "x.png"}, nil)
		text.Format(txn, 0, 3, map[string]any{"bold": true, "link": map[string]any{"href": "a"}})
		arr.Insert(txn, 0, []any{1, "two", []byte{3}, map[string]any{"four": 4.5}, nil, true})
		m.Set(txn, "bin", []byte{1, 2, 3})
		m.Set(txn, "sub", New())
		m.Set(txn, "nested", NewMapPrelim())
		arr.Insert(txn, 1, []any{7, 8})
	})
	doc.Transact(func(txn *Transaction) { text.Delete(txn, 1, 2) })
	second := New(WithClientID(1))
	if err := ApplyUpdateV1(second, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	secondText := second.GetText("t")
	second.Transact(func(txn *Transaction) { secondText.Insert(txn, 0, "z", nil) })
	count(t, EncodeStateAsUpdateV1(second, nil))
}
