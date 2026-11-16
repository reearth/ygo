package crdt

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"testing"
	"time"
)

// Update sequences that give one struct ID two contents, with the text of
// root "t" and the state vector Yjs 13.6.32 ends with after applying them in
// order to a document with GC off. Each seed represents a group of generated
// sequences in which the pending queue did not merge as Yjs merges its pending
// update, or a struct the document held skipped its missing dependencies.
var pendingMergeCases = []struct {
	name     string
	text, sv string
	updates  []string
}{
	{"seed 82", "aabcabcdecdefghiabc", "5:3,7:1,9:15,", []string{"01010903000100", "0101090a840500016100", "010105008407000361626300", "01020903840700096162636465666768698407000361626300", "01010300841400016100", "0201070004010174016101090084050005616263646500"}},
	{"seed 595", "abcd", "9:4,", []string{"01010903000100", "01010901000300", "01011409000100", "01010901840500046162636400", "01010302840500016100", "0201090004010174046162636401070104010174016100"}},
	{"seed 411", "aaaababcdeefghi", "5:2,7:1,9:12,", []string{"010209018407000961626364656667686984070002616200", "01010901000100", "0101090a000100", "0102050004010174016104010174016100", "01010300841400016100", "020107000401017401610109000401017405616263646500"}},
	{"seed 348", "aaabcdefghicdefghbcde", "5:16,7:5,", []string{"010105080401017408616263646566676800", "010105018407000961626364656667686900", "01010904000100", "0101090a840500016100", "01010500840700016100", "01010301841400016100", "020107000401017405616263646501050004010174016100"}},
	{"seed 4592", "aa", "3:3,5:12,7:1,", []string{"010105038407000961626364656667686900", "0101090a000100", "010205008407000161000400", "010209030002840702016100", "02010300000301070004010174016100"}},
	{"seed 515", "abca", "7:4,9:10,", []string{"010109018407000961626364656667686900", "01010901000100", "010103008414000361626300", "0101090a840500016100", "020207000401017401610401017403616263010900000300"}},
	{"seed 5851", "abcdea", "3:1,9:5,20:1,", []string{"01010901000100", "01020901840700096162636465666768698407000361626300", "01010500840700016100", "0101070a000100", "01010300000100", "020109000401017405616263646501140004010174016100"}},
	{"seed 14023", "a", "5:1,7:1,9:10,", []string{"010109018407000961626364656667686900", "01010901000100", "01010500000100", "01010900000200", "020109000401017405616263646501070004010174016100"}},
	{"seed 6809", "", "5:1,7:1,9:10,", []string{"010109018407000961626364656667686900", "01010909840500016100", "01010901000100", "01010500840700016100", "01010900000100", "0201090004010174056162636465010700840900016100"}},
	{"seed 40423", "aabc", "3:1,7:1,9:12,", []string{"01010903000100", "01010700840700016100", "0101030004010174016100", "010109038407000961626364656667686900", "0202090000030401017403616263010700000100", "010109098414000361626300"}},
	{"seed 42619", "aababcdefghi", "3:1,5:1,7:2,9:9,", []string{"010109008407000961626364656667686900", "01010904040101740361626300", "0101090a04010174016100", "01010500840700016100", "020107000401017402616201050084050205616263646500", "01010300000100"}},
	{"seed 35743", "abcaabc", "5:1,7:4,9:9,", []string{"01010901000300", "010209030003000100", "010109068405000361626300", "0101090a04010174016100", "01010500840900016100", "01010900040101740361626300", "02020700000104010174036162630109020401017405616263646500"}},
	// A struct the merge cuts takes its parent from its new origin.
	{"cut parent", "ABCDz", "5:1,9:5,", []string{"01010901040007000362636400", "0101090084050002585900", "01010904840903017a00", "0101090004010174044142434400", "0101050004010175015000"}},
	// A covered struct that waits parks with the client's structs after it,
	// and the merge cuts the queue by them.
	{"covered after a wait", "ABCDEfgh", "5:1,9:8,", []string{"010109028407000663646566676800", "010109000401017405414243444500", "0102090084050001788409000379797900", "0101050004010175015000"}},
	// An item naming its own client past its clock waits on another client's
	// missing struct first, then is refused when that struct arrives.
	{"own reference behind a missing client", "a", "3:1,5:1,", []string{"01010300c4030005000358595a00", "01010500000100", "0101030004010174016100"}},
	{"own reference behind a missing item", "ar", "3:1,9:1,", []string{"01010300c4030009000358595a00", "0101090004010174017200", "0101030004010174016100"}},
	// Queue structs between two windows of one client set the decoder order at
	// the tie in the second window.
	{"structs between windows", "SabcdefGy", "5:9,", []string{"010805018405000161840501016284050201638405030164840504016584050501660a01840507017800", "0103050284050101700a05840507017900", "0101050004010174015300", "01010507840506014700"}},
}

func svText(sv StateVector) string {
	clients := make([]ClientID, 0, len(sv))
	for c := range sv {
		clients = append(clients, c)
	}
	slices.Sort(clients)
	s := ""
	for _, c := range clients {
		s += fmt.Sprintf("%d:%d,", c, sv[c])
	}
	return s
}

// The pending queue and the drain end where Yjs does on updates that give one
// struct ID two contents, in V1 and V2. An update that fails is still applied
// as far as it goes, and the next one follows, as Yjs goes on after a throw.
func TestUnit_PendingMerge_DuplicateStructIDsEndAsInYjs(t *testing.T) {
	for _, tc := range pendingMergeCases {
		for _, v2 := range []bool{false, true} {
			doc := New(WithClientID(100), WithGC(false))
			for _, h := range tc.updates {
				u, err := hex.DecodeString(h)
				if err != nil {
					t.Fatal(err)
				}
				_ = applyVersioned(t, doc, v2, u) // an update Yjs throws on is refused here
			}
			if got := doc.GetText("t").ToString(); got != tc.text {
				t.Errorf("%s v2=%v: text %q, want %q", tc.name, v2, got, tc.text)
			}
			if got := svText(doc.StateVector()); got != tc.sv {
				t.Errorf("%s v2=%v: state vector %s, want %s", tc.name, v2, got, tc.sv)
			}
		}
	}
}

// An item that names a struct of its own client past that client's clock is
// refused, after the structs Yjs integrates before it throws.
func TestUnit_PendingMerge_AFutureOwnReferenceIsRefused(t *testing.T) {
	// Client 9's "abcde" under root "t", then client 3's "a" with origin 3@2.
	u, err := hex.DecodeString("0201090004010174056162636465010300840302016100")
	if err != nil {
		t.Fatal(err)
	}
	for _, v2 := range []bool{false, true} {
		doc := New(WithClientID(100), WithGC(false))
		if err := applyVersioned(t, doc, v2, u); !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("v2=%v: err %v, want ErrInvalidUpdate", v2, err)
		}
		if got := doc.GetText("t").ToString(); got != "abcde" {
			t.Fatalf("v2=%v: text %q, want %q", v2, got, "abcde")
		}
		if got := svText(doc.StateVector()); got != "9:5," {
			t.Fatalf("v2=%v: state vector %s, want 9:5,", v2, got)
		}
		if got := doc.PendingStats().Items; got != 0 {
			t.Fatalf("v2=%v: %d parked, want none", v2, got)
		}
	}
}

func applyHexes(t *testing.T, doc *Doc, v2 bool, hexes ...string) error {
	t.Helper()
	var last error
	for _, h := range hexes {
		u, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		last = applyVersioned(t, doc, v2, u)
	}
	return last
}

// A GC struct that overlaps or touches a parked GC struct merges into it, as
// in Yjs, so it adds nothing to a full queue.
func TestUnit_PendingMerge_GCStructsMerge(t *testing.T) {
	for _, second := range []string{"01010201000200", "01010202000100"} { // GC 2@1+2, GC 2@2+1
		for _, v2 := range []bool{false, true} {
			doc := New(WithClientID(100), WithMaxPendingItems(1))
			if err := applyHexes(t, doc, v2, "01010201000100", second); err != nil { // GC 2@1+1 first
				t.Fatalf("%s v2=%v: %v", second, v2, err)
			}
			if items := doc.store.pending.heldItems(); len(items) != 1 || items[0].ID.Clock != 1 || items[0].Content.Len() != 2 {
				t.Fatalf("%s v2=%v: parked %d structs, want one GC struct 2@1+2", second, v2, len(items))
			}
		}
	}
}

// The merge cuts a parked root-text item behind a GC struct, so its origin
// becomes that GC struct, and it integrates as a GC struct, as in Yjs.
func TestUnit_PendingMerge_ACutItemBehindAGCStructIntegratesAsGC(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		doc := New(WithClientID(100), WithGC(false))
		// Client 3: "yz" at clock 2, then GC structs at clocks 1-2 and 0.
		if err := applyHexes(t, doc, v2, "010103020401017402797a00", "01010301000200", "01010300000100"); err != nil {
			t.Fatalf("v2=%v: %v", v2, err)
		}
		if text := doc.GetText("t"); text.Len() != 0 || text.ToString() != "" {
			t.Fatalf("v2=%v: text %q of length %d, want none", v2, text.ToString(), text.Len())
		}
		if got := svText(doc.StateVector()); got != "3:4," {
			t.Fatalf("v2=%v: state vector %s, want 3:4,", v2, got)
		}
		// The encoded state holds the cut struct as a GC struct too.
		replay := New(WithClientID(101), WithGC(false))
		if err := ApplyUpdateV1(replay, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
			t.Fatalf("v2=%v: replaying the state: %v", v2, err)
		}
		if text := replay.GetText("t"); text.Len() != 0 {
			t.Fatalf("v2=%v: the state replays to text %q", v2, text.ToString())
		}
	}
}

// The cap check counts what the park adds: as in Yjs, a covered struct that
// waits parks with every struct of its client after it, covered or not.
func TestUnit_PendingMerge_TheCapCountsACoveredStructThatWaits(t *testing.T) {
	// Client 3's "ab", then client 3's "x" at 0 waiting on client 9, "y" at 1
	// and "z" at 2.
	const initial, update = "010103000401017402616200", "01030300840900017804010174017904010174017a00"
	for _, v2 := range []bool{false, true} {
		doc := New(WithClientID(100), WithMaxPendingItems(3))
		if err := applyHexes(t, doc, v2, initial, update); err != nil {
			t.Fatalf("v2=%v: an update with exactly its room: %v", v2, err)
		}
		if got := doc.PendingStats().Items; got != 3 {
			t.Fatalf("v2=%v: parked %d, want 3", v2, got)
		}
		tight := New(WithClientID(100), WithMaxPendingItems(2))
		if err := applyHexes(t, tight, v2, initial); err != nil {
			t.Fatal(err)
		}
		before := EncodeStateAsUpdateV1(tight, nil)
		if err := applyHexes(t, tight, v2, update); !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("v2=%v: one short: err %v, want ErrInvalidUpdate", v2, err)
		}
		if string(EncodeStateAsUpdateV1(tight, nil)) != string(before) {
			t.Fatalf("v2=%v: the refused update changed the document", v2)
		}
	}
}

// Over a lowered cap, a struct that takes a parked struct's place applies, as
// it adds nothing, and one that would add a struct is refused before any change.
func TestUnit_PendingMerge_AQueueOverALoweredCapDoesNotGrow(t *testing.T) {
	const seed = "0102030104010174017804010174017900" // client 3: "x" at 1, "y" at 2
	for _, v2 := range []bool{false, true} {
		doc := New(WithClientID(100))
		if err := applyHexes(t, doc, v2, seed); err != nil {
			t.Fatal(err)
		}
		doc.SetMaxPendingItems(1)
		if err := applyHexes(t, doc, v2, "01010301000100"); err != nil { // GC 3@1
			t.Fatalf("v2=%v: the GC copy: %v", v2, err)
		}
		if items := doc.store.pending.heldItems(); len(items) != 2 || !slices.ContainsFunc(items, isGCStruct) {
			t.Fatalf("v2=%v: parked %d, want 2 with the GC copy", v2, len(items))
		}
		before := EncodeStateAsUpdateV1(doc, nil)
		if err := applyHexes(t, doc, v2, "01010304000100"); !errors.Is(err, ErrInvalidUpdate) { // GC 3@4
			t.Fatalf("v2=%v: a new struct: err %v, want ErrInvalidUpdate", v2, err)
		}
		if string(EncodeStateAsUpdateV1(doc, nil)) != string(before) {
			t.Fatalf("v2=%v: the refused update changed the document", v2)
		}
	}
}

// A struct sent again over a large queue merges with the few queue structs
// it touches, so its park allocates about what a small queue's does.
func TestUnit_PendingMerge_AResendOverALargeQueueAllocatesLittle(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations")
	}
	const n = 50_000
	room := parkedRun(t, n)
	// Each resend is a GC copy of a parked item, which takes its place.
	const runs = 200
	ups := make([][]byte, runs+1)
	for i := range ups {
		ups[i] = structsV1(map[ClientID][]*Item{2: {gcStruct(2, uint64(1+i*97%n), 1)}})
	}
	if err := ApplyUpdateV1(room, ups[runs], nil); err != nil { // builds the queue's index
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for _, u := range ups[:runs] {
		if err := ApplyUpdateV1(room, u, nil); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 64<<10 {
		t.Fatalf("a resend allocates %d bytes over a queue of %d; one queue copy is %d", per, n, n*8)
	}
	if got := room.PendingStats().Items; got != n {
		t.Fatalf("parked %d, want %d", got, n)
	}
}

// structsIndexed is each client's structs in c, the struct index of p, after
// checking that each names its slot in p's queue, and the index's clients.
func structsIndexed(t *testing.T, p *pendingUpdate, c *pendingCover) (map[ClientID][]*Item, []ClientID) {
	t.Helper()
	structs := make(map[ClientID][]*Item, len(c.structs))
	for client, list := range c.structs {
		for _, q := range list.slice() {
			if p.items[q.slot] != q.Item {
				t.Fatalf("struct %d@%d names slot %d, which holds another", client, q.ID.Clock, q.slot)
			}
			structs[client] = append(structs[client], q.Item)
		}
	}
	return structs, c.clients.slice()
}

// plan reads only windows of each arrival client's queue structs, and keeps
// what the merge over the whole queue keeps, which is Yjs's merge, also for
// arrivals spread over a long queue, and over clients held past seqFlat.
func TestUnit_PendingMerge_AWindowKeepsWhatTheWholeQueueKeeps(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	run := func(c ClientID, n, gap, maxLen int) []*Item {
		items := make([]*Item, 0, n)
		clock := uint64(rng.IntN(4))
		for range n {
			clock += uint64(rng.IntN(gap + 1))
			l := 1 + rng.IntN(maxLen)
			it := &Item{ID: ID{Client: c, Clock: clock}, Content: NewContentDeleted(uint64(l)), Deleted: true}
			if rng.IntN(2) == 0 {
				it = &Item{ID: ID{Client: c, Clock: clock}, Origin: &ID{Client: 99}, Content: NewContentDeleted(uint64(l))}
			}
			items = append(items, it)
			clock += uint64(l)
		}
		return items
	}
	cases := 30000
	if raceEnabled {
		cases = 3000 // the race detector makes each case about five times slower
	}
	for n := range cases {
		gap := 2 - n%3
		var held, arrived []*Item
		for c := ClientID(8); c >= 3; c-- {
			sparse := n%2 == 1 // a long queue, and arrivals spread over it
			if rng.IntN(3) > 0 {
				switch {
				case sparse && n%10 == 9 && !raceEnabled: // the race detector makes a long queue slow
					held = append(held, run(c, seqFlat+rng.IntN(seqFlat), gap, 3)...)
				case sparse:
					held = append(held, run(c, 10+rng.IntN(100), gap, 3)...)
				default:
					held = append(held, run(c, rng.IntN(12), gap, 3)...)
				}
			}
			if rng.IntN(2) == 0 {
				if sparse {
					arrived = append(arrived, run(c, 1+rng.IntN(15), 20, 10)...)
				} else {
					arrived = append(arrived, run(c, 1+rng.IntN(3), gap, 3)...)
				}
			}
		}
		if len(held) == 0 || len(arrived) == 0 {
			continue
		}
		rng.Shuffle(len(held), func(i, j int) { held[i], held[j] = held[j], held[i] })
		p := &pendingUpdate{items: held}
		pl := p.plan(refsOf(arrived))

		all, in := refsOf(held), refsOf(arrived)
		ho, ao := mergeOrder(all), mergeOrder(in)
		wantHeld, wantArrived := make([]mergeKeep, len(all)), make([]mergeKeep, len(in))
		yjsMerge(all, in, wantHeld, wantArrived, func(ClientID) bool { return false })
		gotHeld := make(map[*Item]mergeKeep, len(pl.held))
		for i, q := range pl.held {
			gotHeld[q.Item] = pl.keepHeld[i]
		}
		for i, q := range ho {
			it := held[q]
			got, ok := gotHeld[it]
			if !ok {
				got = mergeKeep{start: it.ID.Clock, end: it.ID.Clock + it.Content.Len()}
			}
			if got != wantHeld[i] {
				t.Fatalf("case %d: queue struct %d@%d kept %+v, the whole queue keeps %+v", n, it.ID.Client, it.ID.Clock, got, wantHeld[i])
			}
		}
		for i, q := range ao {
			if pl.keepArrived[q] != wantArrived[i] {
				t.Fatalf("case %d: arrival %d@%d kept %+v, the whole queue keeps %+v", n, arrived[q].ID.Client, arrived[q].ID.Clock, pl.keepArrived[q], wantArrived[i])
			}
		}
		// After the park, the index is what a rebuild from the queue gives.
		out := p.apply(pl, arrived, n%4 == 0)
		from := len(p.items)
		p.items = append(p.items, out...)
		p.addCover(out, from)
		got, gotClients := structsIndexed(t, p, p.structIndex())
		rebuilt := &pendingUpdate{items: p.heldItems()}
		want, wantClients := structsIndexed(t, rebuilt, rebuilt.structIndex())
		if !maps.EqualFunc(got, want, slices.Equal) || !slices.Equal(gotClients, wantClients) {
			t.Fatalf("case %d: the index after the park is not the queue's", n)
		}
	}
}

// A GC struct that takes in a long chain of parked GC structs, with items
// inside the chain, reads the chain once, whatever the items.
func TestPerf_PendingMerge_ItemsInsideATakenChainReadItOnce(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	clock := threadCPUClock(t)
	const n = 20000
	park := func(k int) time.Duration {
		room := New(WithClientID(1))
		chain := make([]*Item, n)
		for i := range chain {
			chain[i] = gcStruct(2, uint64(i+2), 1)
		}
		if err := ApplyUpdateV1(room, structsV1(map[ClientID][]*Item{2: chain}), nil); err != nil {
			t.Fatal(err)
		}
		update := append(make([]*Item, 0, 1+k), gcStruct(2, 1, 1))
		for i := range k {
			update = append(update, &Item{ID: ID{Client: 2, Clock: uint64(2 + (i+1)*(n/(k+1)))}, Origin: &ID{Client: 99}, Content: NewContentDeleted(1)})
		}
		u := structsV1(map[ClientID][]*Item{2: update})
		return clock(func() {
			if err := ApplyUpdateV1(room, u, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	var none, many time.Duration = math.MaxInt64, math.MaxInt64
	for range 3 {
		none, many = min(none, park(0)), min(many, park(256))
	}
	t.Logf("a GC struct taking in %d parked GC structs: %v; with 256 items inside: %v (%.1f×)", n, none, many, float64(many)/float64(none))
	if many > 3*none {
		t.Fatalf("items inside the chain multiply the park's work: %v with 256, %v with none", many, none)
	}
}

// GC copies that take the place of every eighth of a client's parked items,
// apart in the queue, leave the client's index in time linear in the queue.
func TestPerf_PendingMerge_SparseReplacementIsLinear(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	clock := threadCPUClock(t)
	replace := func(n int) time.Duration {
		room := New(WithClientID(1), WithMaxPendingItems(n))
		items, copies := make([]*Item, n), make([]*Item, 0, n/8)
		for i := range items {
			// Items at even clocks, a gap before each, and a GC copy over the
			// gap and the item for every eighth.
			items[i] = &Item{ID: ID{Client: 2, Clock: uint64(2 + 2*i)}, Origin: &ID{Client: 99}, Content: NewContentDeleted(1)}
			if i%8 == 0 {
				copies = append(copies, gcStruct(2, uint64(1+2*i), 2))
			}
		}
		if err := ApplyUpdateV1(room, structsV1(map[ClientID][]*Item{2: items}), nil); err != nil {
			t.Fatal(err)
		}
		update := structsV1(map[ClientID][]*Item{2: copies})
		took := clock(func() {
			if err := ApplyUpdateV1(room, update, nil); err != nil {
				t.Fatal(err)
			}
		})
		gcs := 0
		for _, it := range room.store.pending.heldItems() {
			if isGCStruct(it) {
				gcs++
			}
		}
		if got := room.PendingStats().Items; got != n || gcs != n/8 {
			t.Fatalf("parked %d with %d GC structs, want %d with %d", got, gcs, n, n/8)
		}
		return took
	}
	const small, large, runs = 2000, 256000, 128
	var s, l time.Duration = math.MaxInt64, math.MaxInt64
	for range 3 {
		var sum time.Duration
		for range runs {
			sum += replace(small)
		}
		s, l = min(s, sum), min(l, replace(large))
	}
	t.Logf("%d parked items, an eighth replaced, as %d queues of %d: %v; as one: %v (%.1f×)", large, runs, small, s, l, float64(l)/float64(s))
	if l > 6*s { // a linear splice measures about 3×, one splice per window about 19×
		t.Fatalf("sparse replacement grows superlinearly: %v for %d, %v for %d queues of %d", l, large, s, runs, small)
	}
}

// GC copies that each take the place of one parked item, one copy per update
// from the lowest clock up, cost time linear in the updates: no update walks
// the queue.
func TestPerf_PendingMerge_OneReplacementPerUpdateIsLinear(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	clock := threadCPUClock(t)
	replace := func(n int) time.Duration {
		room := New(WithClientID(1), WithMaxPendingItems(n))
		items, copies := make([]*Item, n), make([][]byte, n)
		for i := range items {
			// Items behind a gap, each with its own GC copy over the gap and it.
			items[i] = &Item{ID: ID{Client: 2, Clock: uint64(4*i + 2)}, Origin: &ID{Client: 99}, Content: NewContentDeleted(1)}
			copies[i] = structsV1(map[ClientID][]*Item{2: {gcStruct(2, uint64(4*i+1), 2)}})
		}
		if err := ApplyUpdateV1(room, structsV1(map[ClientID][]*Item{2: items}), nil); err != nil {
			t.Fatal(err)
		}
		took := clock(func() {
			for _, u := range copies {
				if err := ApplyUpdateV1(room, u, nil); err != nil {
					t.Fatal(err)
				}
			}
		})
		held := room.store.pending.heldItems()
		if len(held) != n || slices.ContainsFunc(held, func(it *Item) bool { return !isGCStruct(it) }) {
			t.Fatalf("parked %d, want %d GC structs", len(held), n)
		}
		return took
	}
	const small, large, runs = 2000, 32000, 16
	var s, l time.Duration = math.MaxInt64, math.MaxInt64
	for range 5 {
		var sum time.Duration
		for range runs {
			sum += replace(small)
		}
		s, l = min(s, sum), min(l, replace(large))
	}
	t.Logf("%d parked items replaced one per update, as %d queues of %d: %v; as one: %v (%.1f×)", large, runs, small, s, l, float64(l)/float64(s))
	if l > 3*s {
		t.Fatalf("one replacement per update grows superlinearly: %v for %d, %v for %d queues of %d", l, large, s, runs, small)
	}
}

// A queue struct between two windows of one client sets the decoder order the
// second window starts with, as in Yjs: the GC copy at 20 merges the queue's
// two GC structs there into one.
func TestUnit_PendingMerge_StructsBetweenWindowsSetTheOrder(t *testing.T) {
	const queue, update = "0106030a000281630003816300010a040002000300", "0105030100020a07816300010a09000700"
	for _, v2 := range []bool{false, true} {
		doc := New(WithClientID(100))
		if err := applyHexes(t, doc, v2, queue, update); err != nil {
			t.Fatalf("v2=%v: %v", v2, err)
		}
		got := make([]string, 0, doc.store.pending.heldStructs())
		for _, it := range doc.store.pending.heldItems() {
			got = append(got, fmt.Sprintf("%d+%d gc=%v", it.ID.Clock, it.Content.Len(), isGCStruct(it)))
		}
		slices.Sort(got)
		want := []string{"1+2 gc=true", "10+2 gc=true", "12+3 gc=false", "15+1 gc=false", "20+7 gc=true"} // Yjs 13.6.32
		if !slices.Equal(got, want) {
			t.Fatalf("v2=%v: queue %v, Yjs's pending update %v", v2, got, want)
		}
	}
}

// parkedRun parks n items of client 2 at clocks [1, n+1), each waiting on
// client 9.
func parkedRun(t testing.TB, n int) *Doc {
	t.Helper()
	room := New(WithClientID(1))
	items := make([]*Item, n)
	for i := range items {
		items[i] = &Item{ID: ID{Client: 2, Clock: uint64(i + 1)}, Origin: &ID{Client: 9}, Content: NewContentDeleted(1)}
	}
	if err := ApplyUpdateV1(room, structsV1(map[ClientID][]*Item{2: items}), nil); err != nil {
		t.Fatal(err)
	}
	return room
}

// One GC struct that takes the place of a client's whole parked run drops
// every struct of it, in time linear in the run.
func TestPerf_PendingMerge_ReplacingAParkedRunIsLinear(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	// Thread CPU time; eight small runs hold as many structs as one large run,
	// so the two cost the same when the work is linear.
	clock := threadCPUClock(t)
	const small, large, runs = 2500, 20000, 8
	replace := func(n int) time.Duration {
		room := parkedRun(t, n)
		gc := structsV1(map[ClientID][]*Item{2: {gcStruct(2, 1, n)}})
		took := clock(func() {
			if err := ApplyUpdateV1(room, gc, nil); err != nil {
				t.Fatal(err)
			}
		})
		if got := room.PendingStats().Items; got != 1 {
			t.Fatalf("parked %d after the replacement, want 1", got)
		}
		return took
	}
	var s, l time.Duration = math.MaxInt64, math.MaxInt64
	for range 5 {
		var sum time.Duration
		for range runs {
			sum += replace(small)
		}
		s, l = min(s, sum), min(l, replace(large))
	}
	t.Logf("%d parked structs replaced as %d runs of %d: %v; as one run: %v (%.1f×)", large, runs, small, s, l, float64(l)/float64(s))
	if l > 3*s {
		t.Fatalf("replacing a parked run grows superlinearly: %v for %d, %v for %d runs of %d", l, large, s, runs, small)
	}
}

// Two clients' arrivals at one clock read, of the first client's queue
// structs past its window, two that stand in for them, not all of them.
func TestUnit_PendingMerge_ASameClockArrivalReadsLittleOfTheClientBefore(t *testing.T) {
	const n = 50_000
	held := make([]*Item, 0, n+1)
	for i := range n {
		held = append(held, &Item{ID: ID{Client: 9, Clock: uint64(i)}, Origin: &ID{Client: 99}, Content: NewContentDeleted(1)})
	}
	held = append(held, &Item{ID: ID{Client: 1}, Origin: &ID{Client: 99}, Content: NewContentDeleted(1)})
	p := &pendingUpdate{items: held}
	pl := p.plan([]mergeRef{{client: 9, clock: 0, end: 1}, {client: 1, clock: 0, end: 1}})
	if len(pl.held) > 4 {
		t.Fatalf("the plan reads %d queue structs for two arrivals", len(pl.held))
	}
}

// A merge keeps the struct index equal to the queue, also when the queue holds
// two structs at one clock, which no park makes.
func TestUnit_PendingMerge_TheIndexFollowsTheQueue(t *testing.T) {
	item := &Item{ID: ID{Client: 2, Clock: 1}, Origin: &ID{Client: 9}, Content: NewContentDeleted(2)}
	p := &pendingUpdate{items: []*Item{item, gcStruct(2, 1, 1)}}
	arrived := []*Item{{ID: ID{Client: 2, Clock: 0}, Origin: &ID{Client: 9}, Content: NewContentDeleted(2)}}
	pl := p.plan(refsOf(arrived))
	run := p.apply(pl, arrived, false)
	from := len(p.items)
	p.items = append(p.items, run...)
	p.addCover(run, from)
	index, _ := structsIndexed(t, p, p.structIndex())
	var indexed []*Item
	for _, list := range index {
		indexed = append(indexed, list...)
	}
	held := p.heldItems()
	if len(indexed) != len(held) {
		t.Fatalf("the index holds %d structs for %d queued", len(indexed), len(held))
	}
	for _, it := range held {
		if !slices.Contains(indexed, it) {
			t.Fatalf("queued %d@%d is not in the index", it.ID.Client, it.ID.Clock)
		}
	}
}

// An item whose right origin is a GC struct integrates as a GC struct, as Yjs
// getMissing makes it, whether it applies at once or after it parks.
func TestUnit_PendingMerge_ARightOriginGCStructMakesAGCStruct(t *testing.T) {
	const text, gc, item = "0101090004010174017200", "01010500000100", "01010300c409000500017800" // 9's "r"; 5's GC; 3's "x" between 9@0 and 5@0
	for _, order := range [][]string{{text, gc, item}, {item, text, gc}} {
		for _, v2 := range []bool{false, true} {
			doc := New(WithClientID(100), WithGC(false))
			if err := applyHexes(t, doc, v2, order...); err != nil {
				t.Fatalf("v2=%v: %v", v2, err)
			}
			if got := doc.GetText("t"); got.ToString() != "r" || got.Len() != 1 {
				t.Fatalf("v2=%v order %v: text %q of length %d, want \"r\"", v2, order, got.ToString(), got.Len())
			}
			if got := svText(doc.StateVector()); got != "3:1,5:1,9:1," {
				t.Fatalf("v2=%v: state vector %s", v2, got)
			}
		}
	}
}

// Clients join and leave the struct index's client list in time linear in
// them: new clients parking at once, and GC copies that take the place of the
// lower half of the clients' parked structs.
func TestPerf_PendingMerge_ManyClientsJoinAndLeaveLinearly(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	clock := threadCPUClock(t)
	parked := func(client ClientID) []*Item {
		return []*Item{{ID: ID{Client: client, Clock: 1}, Origin: &ID{Client: 9}, Content: NewContentDeleted(1)}}
	}
	// room parks client 2 and queued, then merges once, which builds the index.
	room := func(queued int) *Doc {
		doc := New(WithClientID(1))
		queue := map[ClientID][]*Item{2: parked(2)}
		for c := range queued {
			queue[ClientID(1000+c)] = parked(ClientID(1000 + c))
		}
		for _, u := range [][]byte{structsV1(queue), structsV1(map[ClientID][]*Item{2: {gcStruct(2, 1, 1)}})} {
			if err := ApplyUpdateV1(doc, u, nil); err != nil {
				t.Fatal(err)
			}
		}
		return doc
	}
	apply := func(doc *Doc, update []byte, want int) time.Duration {
		took := clock(func() {
			if err := ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatal(err)
			}
		})
		if got := doc.PendingStats().Items; got != want {
			t.Fatalf("parked %d, want %d", got, want)
		}
		return took
	}
	join := func(n int) time.Duration {
		// Clients in descending order, as Yjs writes them, each joining in
		// front of the ones before.
		update := binary.AppendUvarint(nil, uint64(n))
		for c := n - 1; c >= 0; c-- {
			one := structsV1(map[ClientID][]*Item{ClientID(1000 + c): parked(ClientID(1000 + c))})
			update = append(update, one[1:len(one)-1]...)
		}
		return apply(room(0), append(update, 0), n+1)
	}
	leave := func(n int) time.Duration {
		update := make(map[ClientID][]*Item, n)
		for c := range n {
			update[ClientID(1000+c)] = []*Item{gcStruct(ClientID(1000+c), 1, 1)}
		}
		return apply(room(2*n), structsV1(update), 2*n+1)
	}
	const small, large, runs = 2000, 32000, 16
	for _, shape := range []struct {
		name string
		run  func(int) time.Duration
	}{{"join", join}, {"leave", leave}} {
		var s, l time.Duration = math.MaxInt64, math.MaxInt64
		for range 5 {
			var sum time.Duration
			for range runs {
				sum += shape.run(small)
			}
			s, l = min(s, sum), min(l, shape.run(large))
		}
		t.Logf("%s: %d clients as %d updates of %d: %v; as one: %v (%.1f×)", shape.name, large, runs, small, s, l, float64(l)/float64(s))
		if l > 3*s {
			t.Fatalf("%s grows superlinearly: %v for %d clients, %v for %d updates of %d", shape.name, l, large, s, runs, small)
		}
	}
}

// A resend over a client's chain of touching GC structs reads the chain only
// as far as the merge takes it in: a copy of the first reads a few structs,
// and a GC struct that takes in the whole chain keeps what the whole-queue
// merge keeps.
func TestUnit_PendingMerge_AResendOverAGCChainReadsWhatItTakesIn(t *testing.T) {
	const n = 50_000
	chain := func() *pendingUpdate {
		held := make([]*Item, n)
		for i := range held {
			held[i] = gcStruct(2, uint64(i+1), 1)
		}
		return &pendingUpdate{items: held}
	}
	if pl := chain().plan([]mergeRef{{client: 2, clock: 1, end: 2, gc: true}}); len(pl.held) > 8 {
		t.Fatalf("a GC copy of the first struct reads %d queue structs", len(pl.held))
	}
	p := chain()
	pl := p.plan([]mergeRef{{client: 2, clock: 0, end: 2, gc: true}})
	all := refsOf(p.items)
	wantHeld, wantArrived := make([]mergeKeep, n), make([]mergeKeep, 1)
	yjsMerge(all, []mergeRef{{client: 2, clock: 0, end: 2, gc: true}}, wantHeld, wantArrived, func(ClientID) bool { return false })
	if pl.keepArrived[0] != wantArrived[0] || len(pl.held) != n {
		t.Fatalf("taking in the chain keeps %+v reading %d structs; the whole-queue merge keeps %+v", pl.keepArrived[0], len(pl.held), wantArrived[0])
	}
	for i := range pl.held {
		if pl.keepHeld[i] != wantHeld[i] {
			t.Fatalf("queue struct %d kept %+v, the whole-queue merge keeps %+v", i, pl.keepHeld[i], wantHeld[i])
		}
	}
}

// Two arrivals of one client far apart read the queue structs near each, not
// those between.
func TestUnit_PendingMerge_ArrivalsFarApartReadLittle(t *testing.T) {
	const n = 50_000
	held := make([]*Item, n)
	for i := range held {
		held[i] = &Item{ID: ID{Client: 2, Clock: uint64(i + 1)}, Origin: &ID{Client: 9}, Content: NewContentDeleted(1)}
	}
	p := &pendingUpdate{items: held}
	pl := p.plan([]mergeRef{{client: 2, clock: 1, end: 2}, {client: 2, clock: n, end: n + 1}})
	if len(pl.held) > 8 {
		t.Fatalf("two arrivals read %d queue structs", len(pl.held))
	}
}
