package crdt

import (
	"cmp"
	"encoding/base64"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// parkedTail returns client 2's per-keystroke updates after client 9's "ab", their combined tail, "ab" itself, and the writer.
func parkedTail(t *testing.T, n int) (keystrokes [][]byte, tail, ab []byte, writer *Doc) {
	t.Helper()
	other := New(WithClientID(9))
	other.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "ab", nil) })
	ab = EncodeStateAsUpdateV1(other, nil)
	writer = New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(writer, ab, nil))
	before := writer.StateVector()
	for i := range n {
		sv := writer.StateVector()
		writer.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 2+i, "x", nil) })
		keystrokes = append(keystrokes, EncodeStateAsUpdateV1(writer, sv))
	}
	return keystrokes, EncodeStateAsUpdateV1(writer, before), ab, writer
}

func applyVersioned(t *testing.T, doc *Doc, v2 bool, update []byte) error {
	t.Helper()
	if !v2 {
		return ApplyUpdateV1(doc, update, nil)
	}
	converted, err := UpdateV1ToV2(update)
	require.NoError(t, err)
	return ApplyUpdateV2(doc, converted, nil)
}

// A resent parked update or stored state parks nothing again, as Yjs merges its pending structs.
func TestUnit_PendingQueue_AResentParkedStructParksOnce(t *testing.T) {
	const n = 5
	keystrokes, tail, ab, writer := parkedTail(t, n)
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1), WithMaxPendingItems(n))
		for _, k := range keystrokes {
			require.NoError(t, applyVersioned(t, room, v2, k), "v2=%v", v2)
		}
		require.Equal(t, n, room.PendingStats().Items, "v2=%v: a cap of exactly the pending count must refuse no resend", v2)
		stored := EncodeStateAsUpdateV1(room, nil)
		for i := range 3 {
			require.NoError(t, applyVersioned(t, room, v2, tail), "v2=%v: tail resend %d", v2, i+1)
			require.NoError(t, applyVersioned(t, room, v2, keystrokes[n-1]), "v2=%v: keystroke resend %d", v2, i+1)
			require.NoError(t, applyVersioned(t, room, v2, stored), "v2=%v: stored state resend %d", v2, i+1)
			require.Equal(t, n, room.PendingStats().Items, "v2=%v after resend %d", v2, i+1)
		}

		// A document loaded from the stored state twice holds the tail once.
		loaded := New(WithClientID(3), WithMaxPendingItems(math.MaxInt))
		require.NoError(t, applyVersioned(t, loaded, v2, stored))
		require.NoError(t, applyVersioned(t, loaded, v2, stored))
		require.Equal(t, n, loaded.PendingStats().Items, "v2=%v", v2)

		require.NoError(t, applyVersioned(t, room, v2, ab), "v2=%v", v2)
		require.Equal(t, writer.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
		require.Zero(t, room.PendingStats().Items, "v2=%v", v2)
	}
}

// A resend that holds the parked keystrokes and a new struct after them
// drops the keystrokes and parks only the new struct.
func TestUnit_PendingQueue_AResendLongerThanTheParkedTailParksItsNewClocks(t *testing.T) {
	const n = 4
	keystrokes, _, ab, writer := parkedTail(t, n)
	before := StateVector{9: 2}
	writer.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 2+n, "yy", nil) })
	longer := EncodeStateAsUpdateV1(writer, before)
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1), WithMaxPendingItems(n+1))
		for _, k := range keystrokes {
			require.NoError(t, applyVersioned(t, room, v2, k), "v2=%v", v2)
		}
		require.NoError(t, applyVersioned(t, room, v2, longer), "v2=%v", v2)
		require.Equal(t, n+1, room.PendingStats().Items, "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, longer), "v2=%v", v2)
		require.Equal(t, n+1, room.PendingStats().Items, "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, ab), "v2=%v", v2)
		require.Equal(t, writer.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
		require.Equal(t, writer.StateVector(), room.StateVector(), "v2=%v", v2)
		require.Zero(t, room.PendingStats().Items, "v2=%v", v2)
	}
}

// A resent deleted struct whose first clocks the queue holds is sliced like any other struct.
func TestUnit_PendingQueue_ASlicedDeletedStructContinuesTheParkedOne(t *testing.T) {
	craft := func(n int) []byte {
		it := &Item{ID: ID{Client: 2, Clock: 0}, Origin: &ID{Client: 9, Clock: 1}, Content: NewContentDeleted(uint64(n)), Deleted: true}
		return encodeStructStoreV1(map[ClientID][]*Item{2: {it}}, newDeleteSet(), nil)
	}
	other := New(WithClientID(9))
	other.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "ab", nil) })
	ab := EncodeStateAsUpdateV1(other, nil)

	room := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(room, craft(3), nil))
	require.NoError(t, ApplyUpdateV1(room, craft(5), nil))
	require.Equal(t, 2, room.PendingStats().Items)
	require.NoError(t, ApplyUpdateV1(room, ab, nil))

	// The room must converge with a document that got only the longer struct.
	direct := New(WithClientID(3))
	require.NoError(t, ApplyUpdateV1(direct, craft(5), nil))
	require.NoError(t, ApplyUpdateV1(direct, ab, nil))
	require.Zero(t, room.PendingStats().Items)
	require.Equal(t, direct.StateVector(), room.StateVector())
	require.Equal(t, direct.GetText("t").ToString(), room.GetText("t").ToString())
	require.Equal(t, buildDeleteSet(direct.store).clients, buildDeleteSet(room.store).clients)
	// The slice continues the parked struct: its origin is the clock before it.
	structs := room.store.clients[2]
	require.Len(t, structs, 2)
	require.Equal(t, &ID{Client: 2, Clock: 2}, structs[1].Origin)
}

// A resend whose one struct ends in clocks the queue holds, and starts in
// clocks the document lacks, parks only the clocks the queue lacks, as Yjs
// does: each clock is parked once.
func TestUnit_PendingQueue_AResendWhoseTailIsParkedParksItsHead(t *testing.T) {
	other := New(WithClientID(9))
	other.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "ab", nil) })
	ab := EncodeStateAsUpdateV1(other, nil)
	writer := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(writer, ab, nil))
	writer.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 2, "xxxxxx", nil) })
	long := EncodeStateAsUpdateV1(writer, StateVector{9: 2})      // one struct, clocks 0-5
	end := EncodeStateAsUpdateV1(writer, StateVector{9: 2, 2: 3}) // the same struct from clock 3
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
		require.NoError(t, applyVersioned(t, room, v2, end), "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, long), "v2=%v", v2)
		var held uint64
		for _, it := range room.store.pending.heldItems() {
			held += it.Content.Len()
		}
		require.Equal(t, uint64(6), held, "v2=%v: each parked clock is held once", v2)
		require.NoError(t, applyVersioned(t, room, v2, ab), "v2=%v", v2)
		require.Equal(t, writer.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
		require.Zero(t, room.PendingStats().Items, "v2=%v", v2)
	}
}

// A held struct that arrives again as an integrable GC struct integrates, as in Yjs, which merges only what an update leaves.
func TestUnit_PendingQueue_AHeldStructThatCanIntegrateIntegrates(t *testing.T) {
	updates := []string{
		// The room parks client 3's entries of client 1's nested array, which it lacks.
		"AQEDAQgAAQACdwF4dwF5AA==",
		// A GC'd peer's state carries those clocks as a GC struct, behind client 3's "c", which waits on client 2's later group.
		"AgIDAIQCAAFjAAIBAgAEAQF0AWICAwEBAgEBAAE=",
		// Client 3 types "z" after "c".
		"AQEDA4QDAAF6AgMBAQIBAQAB",
		"AQEBAAcBAWEAAA==",
	}
	// The texts Yjs 13.6 shows after each update.
	yjs := []string{"", "bc", "bcz", "bcz"}
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(9))
		for i, b64 := range updates {
			u, err := base64.StdEncoding.DecodeString(b64)
			require.NoError(t, err)
			require.NoError(t, applyVersioned(t, room, v2, u), "v2=%v update %d", v2, i)
			require.Equal(t, yjs[i], room.GetText("t").ToString(), "v2=%v after update %d", v2, i)
		}
	}
}

// typeAs makes client type s at pos of the text name, on a copy of world,
// and applies the edit to world as one update.
func typeAs(t *testing.T, world *Doc, client ClientID, name string, pos int, s string) {
	t.Helper()
	d := New(WithClientID(client))
	require.NoError(t, ApplyUpdateV1(d, EncodeStateAsUpdateV1(world, nil), nil))
	d.Transact(func(txn *Transaction) { txn.GetText(name).Insert(txn, pos, s, nil) })
	require.NoError(t, ApplyUpdateV1(world, EncodeStateAsUpdateV1(d, world.StateVector()), nil))
}

// Re-sent structs in a group whose head can never integrate within the update
// do not count toward the cap, beside a new struct.
func TestUnit_PendingBudget_HeldStructsInAPermanentGroupDoNotCount(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		world := New(WithClientID(100))
		typeAs(t, world, 9, "t", 0, "ab")
		for i := range 4 {
			typeAs(t, world, 2, "t", 2+i, "x")
		}
		parked := EncodeStateAsUpdateV1(world, StateVector{9: 2})
		typeAs(t, world, 2, "t", 6, "x")
		typeAs(t, world, 4, "u", 0, "P")
		typeAs(t, world, 3, "u", 1, "m")
		typeAs(t, world, 3, "u", 2, "m")
		resend := EncodeStateAsUpdateV1(world, StateVector{9: 2})
		ab := EncodeStateAsUpdateV1(world, StateVector{2: 100, 3: 100, 4: 100})

		room := New(WithClientID(1), WithMaxPendingItems(6))
		require.NoError(t, applyVersioned(t, room, v2, parked))
		require.Equal(t, 4, room.PendingStats().Items, "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, resend), "v2=%v", v2)
		require.Equal(t, 5, room.PendingStats().Items, "v2=%v", v2)
		require.Equal(t, "Pmm", room.GetText("u").ToString(), "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, ab), "v2=%v", v2)
		require.Zero(t, room.PendingStats().Items, "v2=%v", v2)
		require.Equal(t, world.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
	}
}

// Re-sent structs do not count toward the cap when the budget walks the
// blocked groups it collected: both scans exceed the room (a three-deep client
// chain), and the parked client's dependency is on the wire but blocked.
func TestUnit_PendingBudget_HeldStructsInACollectedGroupDoNotCount(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		world := New(WithClientID(100))
		typeAs(t, world, 10, "t", 0, "Q")
		typeAs(t, world, 9, "t", 1, "ab")
		for i := range 4 {
			typeAs(t, world, 2, "t", 3+i, "x")
		}
		parked := EncodeStateAsUpdateV1(world, StateVector{10: 1, 9: 2})
		typeAs(t, world, 2, "t", 7, "x")
		typeAs(t, world, 5, "u", 0, "P")
		typeAs(t, world, 4, "u", 1, "o")
		typeAs(t, world, 3, "u", 2, "m")
		resend := EncodeStateAsUpdateV1(world, StateVector{10: 1})
		q := EncodeStateAsUpdateV1(world, StateVector{2: 100, 3: 100, 4: 100, 5: 100, 9: 100})

		room := New(WithClientID(1), WithMaxPendingItems(6))
		require.NoError(t, applyVersioned(t, room, v2, parked))
		require.Equal(t, 4, room.PendingStats().Items, "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, resend), "v2=%v", v2)
		require.Equal(t, 6, room.PendingStats().Items, "v2=%v", v2)
		require.Equal(t, "Pom", room.GetText("u").ToString(), "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, q), "v2=%v", v2)
		require.Zero(t, room.PendingStats().Items, "v2=%v", v2)
		require.Equal(t, world.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
	}
}

// A struct typed at the start of the text has a right origin and no origin;
// its slice continues the parked struct, as Yjs sliceStruct does.
func TestUnit_PendingQueue_ASlicedStructWithOnlyARightOriginContinuesTheParkedOne(t *testing.T) {
	other := New(WithClientID(9))
	other.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "ab", nil) })
	ab := EncodeStateAsUpdateV1(other, nil)
	typed := func(s string) (*Doc, []byte) {
		w := New(WithClientID(2))
		require.NoError(t, ApplyUpdateV1(w, ab, nil))
		w.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, s, nil) })
		return w, EncodeStateAsUpdateV1(w, StateVector{9: 2})
	}
	_, short := typed("xxxx")
	writer, long := typed("xxxxyy")
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
		require.NoError(t, applyVersioned(t, room, v2, short))
		require.NoError(t, applyVersioned(t, room, v2, long))
		require.Equal(t, 2, room.PendingStats().Items, "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, ab))
		require.Zero(t, room.PendingStats().Items)
		require.Equal(t, writer.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
		require.Equal(t, EncodeStateAsUpdateV1(writer, nil), EncodeStateAsUpdateV1(room, nil), "v2=%v", v2)
	}
}

func structsV1(m map[ClientID][]*Item) []byte {
	return encodeStructStoreV1(m, newDeleteSet(), nil)
}

func gcStruct(client ClientID, clock uint64, n int) *Item {
	return &Item{ID: ID{Client: client, Clock: clock}, Content: NewContentDeleted(uint64(n)), Deleted: true}
}

// waitingFor9 is a deleted item of client 2 at clock that waits for client 9.
func waitingFor9(clock uint64) *Item {
	return &Item{ID: ID{Client: 2, Clock: clock}, Origin: &ID{Client: 9, Clock: 0}, Content: NewContentDeleted(1)}
}

// The queue holds client 2's clock 1 as an item that waits for client 9; a GC
// copy of clock 1 arrives, then client 2's clock 0. Where a GC struct and an
// item start at one clock, Yjs's merge keeps the arriving one, so the queue
// holds the GC copy alone. It waits only for clock 0, so it integrates then.
func TestUnit_PendingQueue_AGCCopyOfAParkedItemTakesItsPlace(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1))
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {waitingFor9(1)}})))
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {gcStruct(2, 1, 1)}})))
		require.Equal(t, 1, room.PendingStats().Items, "v2=%v", v2)
		require.True(t, isGCStruct(room.store.pending.heldItems()[0]), "v2=%v: the GC copy is parked", v2)
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {gcStruct(2, 0, 1)}})))
		require.Equal(t, uint64(2), room.StateVector().Clock(2), "v2=%v", v2)
	}
}

// A GC struct [1,3) whose tail the queue holds as an item parks whole, so all
// of it integrates once clock 0 arrives.
func TestUnit_PendingQueue_AGCStructWhoseTailIsAParkedItemParksWhole(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1))
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {waitingFor9(2)}})))
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {gcStruct(2, 1, 2)}})))
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {gcStruct(2, 0, 1)}})))
		require.Equal(t, uint64(3), room.StateVector().Clock(2), "v2=%v", v2)
	}
}

// At a full queue, a GC copy of the parked item takes its place and adds
// nothing, as in Yjs's merge, so the update applies; a GC struct at a clock the
// queue lacks adds one, and the budget refuses that update before client 5's
// "k" integrates. V1 only: the V1 wire puts client 2 first, so the budget runs
// first.
func TestUnit_PendingBudget_AGCCopyOfAParkedItemAddsNothing(t *testing.T) {
	typist := New(WithClientID(5))
	typist.Transact(func(txn *Transaction) { txn.GetText("u").Insert(txn, 0, "k", nil) })
	for _, tc := range []struct {
		clock  uint64
		refuse bool
	}{{1, false}, {2, true}} {
		room := New(WithClientID(1), WithMaxPendingItems(1))
		require.NoError(t, ApplyUpdateV1(room, structsV1(map[ClientID][]*Item{2: {waitingFor9(1)}}), nil))
		update, err := MergeUpdatesV1(structsV1(map[ClientID][]*Item{2: {gcStruct(2, tc.clock, 1)}}), EncodeStateAsUpdateV1(typist, nil))
		require.NoError(t, err)
		if tc.refuse {
			require.ErrorIs(t, ApplyUpdateV1(room, update, nil), ErrInvalidUpdate)
			require.Empty(t, room.GetText("u").ToString(), "refused by the budget, before any integration")
		} else {
			require.NoError(t, ApplyUpdateV1(room, update, nil))
			require.Equal(t, "k", room.GetText("u").ToString())
		}
		require.Equal(t, 1, room.PendingStats().Items, "clock %d", tc.clock)
	}
}

// The same, in the walk over collected groups: a three-deep client chain
// (3 on 4 on 5) makes both scans exceed the room, and client 2's group (a
// waiting item, then a GC copy of the parked clock 1) waits for client 9,
// which is on the wire but waits for the absent client 10. The update parks
// client 9's struct and client 2's two, and the copy takes the parked item's
// place, so the queue grows by two, as in Yjs: a room of two applies it, a room
// of one refuses it before any change. V1 only, as above.
func TestUnit_PendingBudget_AGCCopyInACollectedGroupMerges(t *testing.T) {
	world := New(WithClientID(100))
	typeAs(t, world, 10, "t", 0, "Q")
	typeAs(t, world, 9, "t", 1, "ab")
	typeAs(t, world, 5, "u", 0, "P")
	typeAs(t, world, 4, "u", 1, "o")
	typeAs(t, world, 3, "u", 2, "m")
	rest := EncodeStateAsUpdateV1(world, StateVector{10: 1})
	head := &Item{ID: ID{Client: 2, Clock: 0}, Origin: &ID{Client: 9, Clock: 1}, Content: NewContentDeleted(1)}
	update, err := MergeUpdatesV1(structsV1(map[ClientID][]*Item{2: {head, gcStruct(2, 1, 1)}}), rest)
	require.NoError(t, err)

	for _, limit := range []int{3, 2} {
		room := New(WithClientID(1), WithMaxPendingItems(limit))
		parked := &Item{ID: ID{Client: 2, Clock: 1}, Origin: &ID{Client: 9, Clock: 1}, Content: NewContentDeleted(1)}
		require.NoError(t, ApplyUpdateV1(room, structsV1(map[ClientID][]*Item{2: {parked}}), nil))
		if limit == 2 {
			require.ErrorIs(t, ApplyUpdateV1(room, update, nil), ErrInvalidUpdate)
			require.Empty(t, room.GetText("u").ToString(), "refused by the budget, before any integration")
			require.Equal(t, 1, room.PendingStats().Items)
			continue
		}
		require.NoError(t, ApplyUpdateV1(room, update, nil))
		require.Equal(t, "Pom", room.GetText("u").ToString())
		require.Equal(t, 3, room.PendingStats().Items)
	}
}

// coverSpans is c's spans as slices.
func coverSpans(c *pendingCover) map[ClientID][]clockSpan {
	out := make(map[ClientID][]clockSpan, len(c.spans))
	for client, s := range c.spans {
		out[client] = s.slice()
	}
	return out
}

// A park adds its spans to the cover one by one, or, for a batch of many
// spans, in one merge per client; the cover then equals the one built from
// scratch, and the batch joins the spans it touches.
func TestUnit_PendingCover_ABatchParkJoinsTheSpanItTouches(t *testing.T) {
	item := func(clock uint64, n int) *Item {
		return &Item{ID: ID{Client: 2, Clock: clock}, Content: NewContentDeleted(uint64(n))}
	}
	const spans = 10_000
	for _, tc := range []struct {
		name string
		k    int
		gaps bool
	}{{"one by one", 50, true}, {"few spans", seqFlat + 1, false}, {"many spans", spans / 16, true}} {
		p := &pendingUpdate{missing: make(StateVector)}
		for i := range uint64(spans) {
			p.items = append(p.items, item(4*i+100, 1))
		}
		p.coverOf()
		// The batch joins the span at 100 from below and the spans at 200
		// and 204 to each other, and adds k structs in the gaps or as one run.
		batch := []*Item{item(99, 1), item(201, 3)}
		for i := range uint64(tc.k) {
			if tc.gaps {
				batch = append(batch, item(4*i+1002, 1))
			} else {
				batch = append(batch, item(1001+i, 1))
			}
		}
		from := len(p.items)
		p.items = append(p.items, batch...)
		p.addCover(batch, from)
		require.Equal(t, sortedSpans(p.items), coverSpans(p.cover), tc.name)
		got := p.cover.spans[2]
		require.Equal(t, clockSpan{99, 101}, got.at(0), "%s: the batch joins the span at clock 100", tc.name)
		require.Equal(t, clockSpan{200, 205}, got.at(got.search(func(s clockSpan) bool { return s.end > 200 })), tc.name)
	}
}

// A batch park that merges as one span touching the end of the cover joins
// the last span, so no two spans of the cover touch, whether the cover is a
// short list or a long one.
func TestUnit_PendingCover_ASingleSpanBatchJoinsTheLastSpan(t *testing.T) {
	item := func(clock uint64, n int) *Item {
		return &Item{ID: ID{Client: 2, Clock: clock}, Content: NewContentDeleted(uint64(n))}
	}
	for _, spans := range []int{seqFlat / 2, 10_000} {
		p := &pendingUpdate{missing: make(StateVector)}
		for i := range uint64(spans) {
			p.items = append(p.items, item(4*i+100, 1))
		}
		p.coverOf()
		last := p.cover.spans[2].at(spans - 1).end
		const k = seqFlat + 1 // a batch, not one by one
		batch := make([]*Item, 0, k)
		for c := range uint64(k) {
			batch = append(batch, item(last+c, 1))
		}
		from := len(p.items)
		p.items = append(p.items, batch...)
		p.addCover(batch, from)
		got := p.cover.spans[2].slice()
		require.Len(t, got, spans)
		require.Equal(t, clockSpan{last - 1, last + k}, got[spans-1])
		for i := 1; i < len(got); i++ {
			require.Greater(t, got[i].start, got[i-1].end, "spans %d and %d touch", i-1, i)
		}
	}
}

// A GC struct parked behind a gap is added to the cover's GC spans, so a
// resend of it parks no second time.
func TestUnit_PendingQueue_AResentParkedGCStructParksOnce(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1))
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {waitingFor9(5)}})))
		for i := range 3 {
			require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {gcStruct(2, 1, 1)}})), "v2=%v resend %d", v2, i)
		}
		require.Equal(t, 2, room.PendingStats().Items, "v2=%v", v2)
	}
}

// sortedSpans gives the same spans as a sort and merge, for items in any order.
func TestUnit_PendingCover_SortedSpansMatchesASortAndMerge(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for range 500 {
		n := rng.IntN(40)
		items := make([]*Item, 0, n)
		for range n {
			it := &Item{ID: ID{Client: ClientID(rng.IntN(3)), Clock: uint64(rng.IntN(60))}, Content: NewContentDeleted(uint64(1 + rng.IntN(4)))}
			if rng.IntN(2) == 0 {
				it.Deleted = true
			} else {
				it.Origin = &ID{Client: 9, Clock: 0}
			}
			items = append(items, it)
		}
		if rng.IntN(2) == 0 {
			slices.SortStableFunc(items, func(a, b *Item) int { return cmp.Compare(a.ID.Clock, b.ID.Clock) })
		}
		want := make(map[ClientID][]clockSpan)
		for _, it := range items {
			want[it.ID.Client] = append(want[it.ID.Client], clockSpan{it.ID.Clock, it.ID.Clock + it.Content.Len()})
		}
		for c, list := range want {
			want[c] = coalesce(list)
		}
		require.Equal(t, want, sortedSpans(items))
	}
}

func TestUnit_PendingCover_Spans(t *testing.T) {
	item := func(clock uint64, n int) *Item {
		return &Item{ID: ID{Client: 1, Clock: clock}, Content: NewContentDeleted(uint64(n))}
	}
	p := &pendingUpdate{missing: make(StateVector)}
	add := func(it *Item) {
		p.items = append(p.items, it)
		p.addCover([]*Item{it}, len(p.items)-1)
	}
	for _, it := range []*Item{item(10, 2), item(4, 2), item(12, 3), item(6, 1), item(20, 1)} {
		add(it)
	}
	require.Equal(t, []clockSpan{{4, 7}, {10, 15}, {20, 21}}, p.coverOf().spans[1].slice())
	for _, tc := range []struct {
		start, end uint64
		item, gc   bool // merges as an item, and as a GC struct
	}{
		{4, 7, true, true}, {3, 5, true, true}, {6, 10, true, true}, {7, 10, false, true}, {8, 9, false, false},
		{14, 16, true, true}, {15, 20, false, true}, {21, 22, false, true}, {0, 4, false, true}, {0, 3, false, false},
	} {
		require.Equal(t, tc.item, p.mergesAny([]*Item{item(tc.start, int(tc.end-tc.start))}), "item [%d,%d)", tc.start, tc.end)
		require.Equal(t, tc.gc, p.merges(1, tc.start, tc.end, true), "GC struct [%d,%d)", tc.start, tc.end)
	}
	require.False(t, p.merges(2, 10, 12, true), "another client")
	require.False(t, (*pendingUpdate)(nil).merges(1, 10, 12, false), "no queue")
	// add keeps the cover in order and out of order, merging what touches.
	add(item(30, 1))
	add(item(25, 1))
	add(item(7, 3))
	require.Equal(t, []clockSpan{{4, 15}, {20, 21}, {25, 26}, {30, 31}}, p.cover.spans[1].slice())
	require.Equal(t, sortedSpans(p.items), coverSpans(p.cover))
}

// A resend whose one struct spans the parked tail and new clocks is sliced at
// the first clock the queue lacks, its origin the clock before, and the room
// converges with the writer.
func TestUnit_PendingQueue_AResentStructSpanningTheParkedTailIsSliced(t *testing.T) {
	other := New(WithClientID(9))
	other.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "ab", nil) })
	ab := EncodeStateAsUpdateV1(other, nil)
	typed := func(s string) (*Doc, []byte) {
		w := New(WithClientID(2))
		require.NoError(t, ApplyUpdateV1(w, ab, nil))
		w.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 2, s, nil) })
		return w, EncodeStateAsUpdateV1(w, StateVector{9: 2})
	}
	_, short := typed("xxxx")       // one struct, clocks 0-3
	writer, long := typed("xxxxyy") // one struct, clocks 0-5
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
		require.NoError(t, applyVersioned(t, room, v2, short))
		require.NoError(t, applyVersioned(t, room, v2, long))
		require.Equal(t, 2, room.PendingStats().Items, "v2=%v", v2)
		require.Equal(t, uint64(4), room.store.pending.heldItems()[1].ID.Clock, "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, ab))
		require.Zero(t, room.PendingStats().Items)
		require.Equal(t, writer.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
		require.Equal(t, EncodeStateAsUpdateV1(writer, nil), EncodeStateAsUpdateV1(room, nil), "v2=%v", v2)
	}
}

// A queue of one client's structs takes the next tail struct without building
// its cover, and a resend of a parked clock builds the cover and parks once.
func TestUnit_PendingQueue_ATailAppendNeedsNoCover(t *testing.T) {
	keystrokes, _, _, _ := parkedTail(t, 5)
	room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
	for _, u := range keystrokes[:4] {
		require.NoError(t, ApplyUpdateV1(room, u, nil))
	}
	require.Equal(t, 4, room.PendingStats().Items)
	require.Nil(t, room.store.pending.cover, "the queue starts with no cover")
	require.True(t, room.store.pending.tailValid)

	require.NoError(t, ApplyUpdateV1(room, keystrokes[4], nil))
	require.Equal(t, 5, room.PendingStats().Items)
	require.Nil(t, room.store.pending.cover, "a tail append builds no cover")
	require.Equal(t, uint64(5), room.store.pending.tail)

	require.NoError(t, ApplyUpdateV1(room, keystrokes[2], nil))
	require.Equal(t, 5, room.PendingStats().Items, "the resent clock parks once")
	require.NotNil(t, room.store.pending.cover, "a resend checks the cover")
}

// A tail append keeps a cover already built, so a later resend sees its clocks.
func TestUnit_PendingQueue_ATailAppendKeepsABuiltCover(t *testing.T) {
	keystrokes, _, _, _ := parkedTail(t, 4)
	room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
	for _, u := range keystrokes[:3] {
		require.NoError(t, ApplyUpdateV1(room, u, nil))
	}
	room.store.pending.coverOf()
	require.NoError(t, ApplyUpdateV1(room, keystrokes[3], nil))
	require.Equal(t, []clockSpan{{0, 4}}, room.store.pending.cover.spans[2].slice())
	require.NoError(t, ApplyUpdateV1(room, keystrokes[3], nil))
	require.Equal(t, 4, room.PendingStats().Items)
}

// Structs that each park in front of every struct parked before them, behind
// a gap, one per update, cost time linear in the updates: no park moves the
// spans after it.
func TestPerf_PendingCover_ParkingInFrontIsLinear(t *testing.T) {
	if testing.Short() || raceEnabled || testing.CoverMode() != "" {
		t.Skip("timing test")
	}
	clock := threadCPUClock(t)
	park := func(n int) time.Duration {
		room := New(WithClientID(1), WithMaxPendingItems(n))
		ups := make([][]byte, n)
		for i := range ups {
			ups[i] = structsV1(map[ClientID][]*Item{2: {{ID: ID{Client: 2, Clock: uint64(4*(n-i) + 2)}, Origin: &ID{Client: 99}, Content: NewContentDeleted(1)}}})
		}
		took := clock(func() {
			for _, u := range ups {
				if err := ApplyUpdateV1(room, u, nil); err != nil {
					t.Fatal(err)
				}
			}
		})
		if got := room.PendingStats().Items; got != n {
			t.Fatalf("parked %d, want %d", got, n)
		}
		return took
	}
	const small, large, runs = 2000, 64000, 32
	var s, l time.Duration = math.MaxInt64, math.MaxInt64
	for range 5 {
		var sum time.Duration
		for range runs {
			sum += park(small)
		}
		s, l = min(s, sum), min(l, park(large))
	}
	t.Logf("%d structs parked each in front, as %d queues of %d: %v; as one: %v (%.1f×)", large, runs, small, s, l, float64(l)/float64(s))
	if l > 3*s {
		t.Fatalf("parking in front grows superlinearly: %v for %d, %v for %d queues of %d", l, large, s, runs, small)
	}
}
