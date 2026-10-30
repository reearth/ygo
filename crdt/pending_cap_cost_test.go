package crdt

import (
	"math/rand/v2"
	"reflect"
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
	// A room too small for the cheap bound to pass, but not for the
	// keystroke's one struct, takes the exact check, whose first pass walks
	// the bytes (scanPendingGroupsV1) and allocates only the state vector it
	// tracks.
	walked := v1
	walked.remaining = len(keystroke)/2 - 1
	if n := testing.AllocsPerRun(100, func() {
		if !walked.mayParkPastRoom(len(keystroke)) {
			t.Fatal("the cheap bound passed; the check was not taken")
		}
		fresh := walked
		if err := fresh.checkBeforeApply(len(keystroke)); err != nil {
			t.Fatal(err)
		}
	}); n > 2 {
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
// included, and read to the end of the struct section it leaves the scan at
// the same delete set as the full scan. The pending budget's lenient readers
// (pendingScanner with lenient set, and v1Frame on a V1 update's bytes) read
// every struct as the validating scanner reads it: the same length, skip and
// dependencies, and they end where it ends. This holds for updates of several
// groups holding text, map values, nested arrays, deletes and skips
// (gappedUpdate), and for every kind of content whose value is on the rest
// cursor (binary, embeds, formats, Any values, subdocuments) in a group before
// the last.
func TestUnit_PendingBudget_V2StructCountIsTheScannedCount(t *testing.T) {
	type read struct {
		length  uint64
		skip    bool
		deps    [3]ID
		numDeps int
	}
	// scan reads every struct of the update through next, after header reads
	// the group count, a group's struct count and its client and clock.
	scan := func(header func() uint64, client func() ClientID, next func() read) (reads []read, want int) {
		clients := header()
		for range clients {
			n := header()
			want += int(n)
			client()
			header()
			for range n {
				reads = append(reads, next())
			}
		}
		return reads, want
	}
	count := func(t *testing.T, v1 []byte) {
		t.Helper()
		v2, err := UpdateV1ToV2(v1)
		if err != nil {
			t.Fatal(err)
		}
		for _, isV2 := range []bool{false, true} {
			update := v1
			if isV2 {
				update = v2
			}
			full := newPendingScanner(update, isV2)
			wantReads, want := scan(full.uint, full.client, func() read {
				l, s, d, n := full.item()
				return read{l, s, d, n}
			})
			if full.err != nil {
				t.Fatal(full.err)
			}
			lenient := newPendingScanner(update, isV2)
			lenient.lenient = true
			gotReads, _ := scan(lenient.uint, lenient.client, func() read {
				l, s, d, n := lenient.item()
				return read{l, s, d, n}
			})
			if lenient.err != nil || !reflect.DeepEqual(gotReads, wantReads) {
				t.Fatalf("V2 %v: the lenient scanner read %v (err %v); the scanner reads %v", isV2, gotReads, lenient.err, wantReads)
			}
			if !isV2 {
				if got, want := lenient.rest.Remaining(), full.rest.Remaining(); got != want {
					t.Fatalf("the lenient scanner ends %d bytes before the end, the scanner %d", got, want)
				}
				f := v1Frame{buf: update}
				frameReads, _ := scan(f.uint, func() ClientID { return ClientID(f.uint()) }, func() read {
					l, s, d, n := f.item()
					return read{l, s, d, n}
				})
				if f.bad || !reflect.DeepEqual(frameReads, wantReads) {
					t.Fatalf("the V1 frame read %v (bad %v); the scanner reads %v", frameReads, f.bad, wantReads)
				}
				if got, want := len(update)-f.p, full.rest.Remaining(); got != want {
					t.Fatalf("the V1 frame ends %d bytes before the end, the scanner %d", got, want)
				}
				continue
			}
			d, err := newV2Decoder(update)
			if err != nil {
				t.Fatal(err)
			}
			f := v2Frame{d: d}
			frameReads, _ := scan(f.uint, f.client, func() read {
				l, s, deps, n := f.item()
				return read{l, s, deps, n}
			})
			if f.err != nil || !reflect.DeepEqual(frameReads, wantReads) {
				t.Fatalf("the V2 frame read %v (err %v); the scanner reads %v", frameReads, f.err, wantReads)
			}
			if got, want := d.restDec.Remaining(), full.rest.Remaining(); got != want {
				t.Fatalf("the V2 frame ends %d bytes before the end, the scanner %d", got, want)
			}
			if d, err = newV2Decoder(update); err != nil {
				t.Fatal(err)
			}
			if got, ok := v2StructCount(d, false); !ok || got != want {
				t.Fatalf("v2StructCount = %d, %v; the scan reads %d structs", got, ok, want)
			}
			d, err = newV2Decoder(update)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := v2StructCount(d, true); !ok || got != want {
				t.Fatalf("v2StructCount to the end = %d, %v; the scan reads %d structs", got, ok, want)
			}
			walked, errWalked := decodeDeleteSetV2(d)
			scanned, errScanned := decodeDeleteSetV2(full.v2)
			if errWalked != nil || errScanned != nil {
				t.Fatalf("the delete set does not read: %v, %v", errWalked, errScanned)
			}
			if !reflect.DeepEqual(walked.clients, scanned.clients) {
				t.Fatalf("the delete set read after the walk is %v, after the full scan %v", walked.clients, scanned.clients)
			}
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
