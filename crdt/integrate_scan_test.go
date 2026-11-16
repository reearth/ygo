package crdt

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"
)

// scanDoc is a document holding one string of 2n characters, written by client 7 in one
// transaction, so the whole string is one item until something splits it.
func scanDoc(t testing.TB, n int) *Doc {
	t.Helper()
	doc := New(WithClientID(7))
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, strings.Repeat("a", 2*n), nil) })
	return doc
}

// leftOriginsOnly is an update of n one-character items from client 9, the i-th naming
// (7, 2i) of scanDoc's string as its left origin and no right origin. Each splits the
// string at its origin and integrates past the string's rest, and every half it leaves
// merges back into the string at commit (tryMergeWithLefts).
func leftOriginsOnly(n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	for i := range n {
		enc.WriteUint8(wireString | flagHasOrigin)
		enc.WriteVarUint(7)
		enc.WriteVarUint(uint64(2 * i))
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

// applyMeasured applies update to doc and reports the bytes the apply
// allocated, how long it took and the error.
func applyMeasured(doc *Doc, update []byte) (uint64, time.Duration, error) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := ApplyUpdateV1(doc, update, nil)
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, took, err
}

// An update that splits a long run many times, and leaves each half where it
// can merge back at commit, costs the run's length once rather than once for
// each half. When each merge appended its half to the run's whole text and
// shifted client 7's store slice, n = 20,000 allocated about 441 MB in one apply.
func TestASplitRunMergedBackAtCommitCostsItsLengthOnce(t *testing.T) {
	const n = 20_000
	doc := scanDoc(t, n)
	allocated, took, err := applyMeasured(doc, leftOriginsOnly(n))
	t.Logf("n=%d: err=%v, allocated %d bytes in %s", n, err, allocated, took)
	if err != nil {
		t.Fatal(err)
	}
	if allocated > 64<<20 {
		t.Fatalf("the apply allocated %d bytes, want under %d", allocated, 64<<20)
	}
	text := doc.GetText("t").ToString()
	if want := strings.Repeat("a", 2*n) + strings.Repeat("x", n); text != want {
		t.Fatalf("the text is %d characters, %q... - want the run, then every inserted character", len(text), text[:min(len(text), 40)])
	}
	if got := len(doc.store.clients[7]); got != 1 {
		t.Fatalf("client 7's run is %d items, want it merged back into 1", got)
	}
	copied := New()
	if err := ApplyUpdateV1(copied, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	if got := copied.GetText("t").ToString(); got != text {
		t.Fatal("the document's state decodes to a different text")
	}
}

// A run split k times and merged back at commit allocates at most k objects
// for the merges, as when each merge joined its half in place: k splits and
// their merges cost at most k objects more than k splits alone.
func TestSplitsMergedBackAtCommitAllocateOnlyTheJoinedText(t *testing.T) {
	const runs = 1000

	// A split alone, at the next clock of a long run each time.
	long := New(WithGC(false), WithClientID(1))
	longText := long.GetText("t")
	long.Transact(func(txn *Transaction) { longText.Insert(txn, 0, strings.Repeat("a", runs+2), nil) })
	longTxn := &Transaction{doc: long, Local: true}
	clock := uint64(0)
	split := testing.AllocsPerRun(runs, func() {
		clock++
		long.store.getItemCleanStart(longTxn, ID{Client: 1, Clock: clock})
		longTxn.mergeStructs = longTxn.mergeStructs[:0]
	})

	for k := 1; k <= 4; k++ {
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			doc := New(WithGC(false), WithClientID(1))
			text := doc.GetText("t")
			doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, "abcdefghij", nil) })
			txn := &Transaction{doc: doc, Local: true}
			splitsAndMerges := testing.AllocsPerRun(runs, func() {
				for i := 1; i <= k; i++ {
					doc.store.getItemCleanStart(txn, ID{Client: 1, Clock: uint64(2 * i)})
				}
				tryMergeWithLefts(txn)
				txn.mergeStructs = txn.mergeStructs[:0]
			})
			if got := len(doc.store.clients[1]); got != 1 {
				t.Fatalf("client 1's run is %d items, want it merged back into 1", got)
			}
			if got := text.ToString(); got != "abcdefghij" {
				t.Fatalf("the text is %q, want %q", got, "abcdefghij")
			}
			if merges := splitsAndMerges - float64(k)*split; merges > float64(k) {
				t.Fatalf("%d merges back allocated %.0f objects (%.0f with their splits, %.0f for each split alone), want at most %d, one joined text each", k, merges, splitsAndMerges, split, k)
			}
		})
	}
}

// Three halves merge back into one run, the first joined in place and the
// others gathered, beside a half an insert keeps apart and runs of another
// type and client; each run keeps every byte once, and each client's store
// slice stays in clock order with every item that remains.
func TestSplitRunsMergedBackAroundAnInsertLeaveEachStoreSliceWhole(t *testing.T) {
	doc := New(WithGC(false), WithClientID(1))
	text, array := doc.GetText("t"), doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		text.Insert(txn, 0, "abcdefghijkl", nil) // client 1, clocks 0-11
		array.Push(txn, []any{1, 2, 3, 4, 5, 6}) // client 1, clocks 12-17
	})
	peer := New(WithClientID(2))
	if err := ApplyUpdateV1(peer, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	peerText := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { peerText.Insert(txn, 12, "KLMNOPQRST", nil) }) // client 2, clocks 0-9
	if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(peer, doc.StateVector()), nil); err != nil {
		t.Fatal(err)
	}

	doc.Transact(func(txn *Transaction) {
		// Split out of clock order and across clients, so the halves reach the
		// commit in an order the merge must sort.
		for _, id := range []ID{{1, 10}, {1, 4}, {2, 5}, {1, 16}, {1, 2}, {1, 8}, {1, 14}, {1, 6}} {
			doc.store.getItemCleanStart(txn, id)
		}
		text.Insert(txn, 8, "X", nil) // client 1, clock 18, between clocks 7 and 8
	})

	requireStoreWhole(t, doc, &text.abstractType, &array.abstractType)
	if got, want := text.ToString(), "abcdefghXijklKLMNOPQRST"; got != want {
		t.Fatalf("the text is %q, want %q", got, want)
	}
	if got, want := fmt.Sprint(array.ToSlice()), "[1 2 3 4 5 6]"; got != want {
		t.Fatalf("the array is %s, want %s", got, want)
	}
	for client, want := range map[ClientID]string{
		1: "0+8:abcdefgh 8+4:ijkl 12+6 18+1:X",
		2: "0+10:KLMNOPQRST",
	} {
		items := doc.store.clients[client]
		got := make([]string, 0, len(items))
		for _, item := range items {
			entry := fmt.Sprintf("%d+%d", item.ID.Clock, item.Content.Len())
			if s, ok := item.Content.(*ContentString); ok {
				entry += ":" + s.Str
			}
			got = append(got, entry)
		}
		if strings.Join(got, " ") != want {
			t.Fatalf("client %d's store slice holds %v, want %s", client, got, want)
		}
	}
	copied := New()
	if err := ApplyUpdateV1(copied, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	if got := copied.GetText("t").ToString(); got != text.ToString() {
		t.Fatalf("the document's state decodes to the text %q", got)
	}
}

// requireStoreWhole requires every client's store slice to run in clock order
// from 0, each item starting where the one before it ends, and every item in
// the lists of roots to be the item the store finds at its clock, which the
// store's binary searches rely on.
func requireStoreWhole(t *testing.T, doc *Doc, roots ...*abstractType) {
	t.Helper()
	for client, items := range doc.store.clients {
		next := uint64(0)
		for i, item := range items {
			if item.ID.Clock != next {
				t.Fatalf("client %d's store slice has clock %d at index %d, want %d", client, item.ID.Clock, i, next)
			}
			next += item.Content.Len()
		}
	}
	for _, root := range roots {
		for item := root.start; item != nil; item = item.Right {
			if found := doc.store.Find(item.ID); found != item {
				t.Fatalf("the store does not hold the listed item %v at its clock", item.ID)
			}
		}
	}
}

// chainedList is a document whose text is l one-character items from clients 5
// and 6 in turn, each typed at the end after the other's, so no two neighbours
// merge and each item's left origin is its left neighbour.
func chainedList(t testing.TB, l int) *Doc {
	t.Helper()
	a, b := New(WithClientID(5)), New(WithClientID(6))
	at, bt := a.GetText("t"), b.GetText("t")
	for i := range l {
		from, to, text := a, b, at
		if i%2 == 1 {
			from, to, text = b, a, bt
		}
		sv := to.StateVector()
		from.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "c", nil) })
		if err := ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, sv), nil); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// originAtTheStart is an update of n one-character items from client 9, each
// naming the first item of chainedList's text as its left origin and no right
// origin. YATA places such an item by scanning right from its origin past every
// item whose own origin lies in what the scan has passed (Item.integrate), and
// in chainedList that is every item to the end, so each of these items scans
// the whole list.
func originAtTheStart(n int) []byte { return originAtTheStartFrom(0, n) }

// originAtTheStartFrom is originAtTheStart with client 9's clocks from clock.
func originAtTheStartFrom(clock uint64, n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(clock)
	for range n {
		enc.WriteUint8(wireString | flagHasOrigin)
		enc.WriteVarUint(5)
		enc.WriteVarUint(0)
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

// An update whose n items each scan a list of l items allocates for the list
// once, not n times: its scans share one map of marks. With two fresh sets per
// scan, l = 2,000 and n = 200 allocated about 30 MB.
func TestAnUpdateWhoseItemsEachScanTheListAllocatesForTheListOnce(t *testing.T) {
	const l, n = 2_000, 200
	doc := chainedList(t, l)
	allocated, took, err := applyMeasured(doc, originAtTheStart(n))
	t.Logf("l=%d n=%d: err=%v, allocated %d bytes in %s", l, n, err, allocated, took)
	if err != nil {
		t.Fatal(err)
	}
	if allocated > 4<<20 {
		t.Fatalf("the apply allocated %d bytes, want under %d", allocated, 4<<20)
	}
	if got, want := doc.GetText("t").ToString(), strings.Repeat("c", l)+strings.Repeat("x", n); got != want {
		t.Fatalf("the text is %d characters, want the list, then every inserted character", len(got))
	}
}

// concurrentKeystrokes is the first state of a two-character text and one
// update for each of peers*n keystrokes into it, each typed at a random place
// by a random one of peers peers; after one keystroke in three, a random peer
// catches up with the typist.
func concurrentKeystrokes(t *testing.T, seed uint64, peers, n int) (state []byte, keystrokes [][]byte) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 1))
	base := New(WithClientID(1))
	baseText := base.GetText("t")
	base.Transact(func(txn *Transaction) { baseText.Insert(txn, 0, "ab", nil) })
	state = EncodeStateAsUpdateV1(base, nil)
	docs := make([]*Doc, peers)
	for p := range docs {
		docs[p] = New(WithClientID(ClientID(10 + p)))
		if err := ApplyUpdateV1(docs[p], state, nil); err != nil {
			t.Fatal(err)
		}
	}
	keystrokes = make([][]byte, 0, peers*n)
	for k := range peers * n {
		typist := docs[rng.IntN(peers)]
		text := typist.GetText("t")
		sv := typist.StateVector()
		// Every keystroke is a distinct character, so the text shows the order.
		char, pos := string(rune(0x4e00+k)), rng.IntN(text.Len()+1)
		typist.Transact(func(txn *Transaction) { text.Insert(txn, pos, char, nil) })
		keystrokes = append(keystrokes, EncodeStateAsUpdateV1(typist, sv))
		if rng.IntN(3) == 0 {
			peer := docs[rng.IntN(peers)]
			if err := ApplyUpdateV1(peer, EncodeStateAsUpdateV1(typist, peer.StateVector()), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	return state, keystrokes
}

// loadText applies updates in order to a new document and returns its text.
func loadText(t *testing.T, updates ...[]byte) string {
	t.Helper()
	doc := New()
	for _, update := range updates {
		if err := ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatal(err)
		}
	}
	return doc.GetText("t").ToString()
}

// One update's conflict scans share the map of marks, and none of them reads
// another's: concurrent keystrokes merged into one update read as when each
// is applied alone.
func TestKeystrokesInOneUpdateReadAsWhenEachIsAppliedAlone(t *testing.T) {
	for seed := range uint64(20) {
		state, keystrokes := concurrentKeystrokes(t, seed, 8, 30)
		merged, err := MergeUpdatesV1(keystrokes...)
		if err != nil {
			t.Fatal(err)
		}
		alone := loadText(t, append([][]byte{state}, keystrokes...)...)
		if together := loadText(t, state, merged); together != alone {
			t.Fatalf("seed %d: the keystrokes as one update read\n%s\nwant, each applied alone,\n%s", seed, together, alone)
		}
	}
}
