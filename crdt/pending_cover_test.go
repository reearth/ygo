package crdt

import (
	"encoding/base64"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// parkedTail is a writer, client 2, that holds client 9's "ab" and typed n
// characters after it one keystroke at a time: keystrokes are its updates,
// tail is everything after "ab" in one update, and ab is client 9's edit.
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

// The same parked update applied again, or the stored state that holds it,
// parks nothing again (Yjs merges its pending structs): the pending count
// stays as it was, and a document whose pending cap is exactly that count
// refuses none of the resends.
func TestUnit_PendingQueue_AResentParkedStructParksOnce(t *testing.T) {
	const n = 5
	keystrokes, tail, ab, writer := parkedTail(t, n)
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1), WithMaxPendingItems(n))
		for _, k := range keystrokes {
			require.NoError(t, applyVersioned(t, room, v2, k), "v2=%v", v2)
		}
		require.Equal(t, n, room.PendingStats().Items, "v2=%v", v2)
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

// A resend of a deleted struct (not a GC placeholder) whose first clocks the
// queue holds is sliced with the clock before the slice as its origin, as any
// other struct is, and the document converges with one that got only the
// longer struct.
func TestUnit_PendingQueue_ASlicedDeletedStructContinuesTheParkedOne(t *testing.T) {
	craft := func(n int) []byte {
		it := &Item{ID: ID{Client: 2, Clock: 0}, Origin: &ID{Client: 9, Clock: 1}, Content: NewContentDeleted(n), Deleted: true}
		return encodeStructStoreV1(map[ClientID][]*Item{2: {it}}, newDeleteSet(), nil, newStructStore())
	}
	other := New(WithClientID(9))
	other.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "ab", nil) })
	ab := EncodeStateAsUpdateV1(other, nil)

	room := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(room, craft(3), nil))
	require.NoError(t, ApplyUpdateV1(room, craft(5), nil))
	require.Equal(t, 2, room.PendingStats().Items)
	require.NoError(t, ApplyUpdateV1(room, ab, nil))

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
		held := 0
		for _, it := range room.store.pending.items {
			held += it.Content.Len()
		}
		require.Equal(t, 6, held, "v2=%v: each parked clock is held once", v2)
		require.NoError(t, applyVersioned(t, room, v2, ab), "v2=%v", v2)
		require.Equal(t, writer.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
		require.Zero(t, room.PendingStats().Items, "v2=%v", v2)
	}
}

// A struct the queue holds, arriving again as a GC struct that can integrate,
// integrates: an update first integrates what it can and only what it leaves
// merges with the queue, as in Yjs. The updates are Yjs-encoded: the room
// parks client 3's entries of client 1's nested array, which it lacks; a
// GC'd peer's state then carries those clocks as a GC struct, behind client
// 3's "c", which waits on client 2's group later in the same update; then
// client 3 types "z" after "c". The texts are what Yjs 13.6 shows after each.
func TestUnit_PendingQueue_AHeldStructThatCanIntegrateIntegrates(t *testing.T) {
	updates := []string{
		"AQEDAQgAAQACdwF4dwF5AA==",
		"AgIDAIQCAAFjAAIBAgAEAQF0AWICAwEBAgEBAAE=",
		"AQEDA4QDAAF6AgMBAQIBAQAB",
		"AQEBAAcBAWEAAA==",
	}
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
	return encodeStructStoreV1(m, newDeleteSet(), nil, newStructStore())
}

func gcStruct(client ClientID, clock uint64, n int) *Item {
	return &Item{ID: ID{Client: client, Clock: clock}, Content: NewContentDeleted(n), Deleted: true}
}

// waitingFor9 is a deleted item of client 2 at clock that waits for client 9.
func waitingFor9(clock uint64) *Item {
	return &Item{ID: ID{Client: 2, Clock: clock}, Origin: &ID{Client: 9, Clock: 0}, Content: NewContentDeleted(1)}
}

// The queue holds client 2's clock 1 as an item that waits for client 9; a GC
// copy of clock 1 arrives, then client 2's clock 0. The GC copy waits only for
// clock 0, so it integrates then, and client 2's clock reaches 2.
func TestUnit_PendingQueue_AGCCopyOfAParkedItemParksAndIntegrates(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		room := New(WithClientID(1))
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {waitingFor9(1)}})))
		require.NoError(t, applyVersioned(t, room, v2, structsV1(map[ClientID][]*Item{2: {gcStruct(2, 1, 1)}})))
		require.Equal(t, 2, room.PendingStats().Items, "v2=%v: the GC copy parks", v2)
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

// At a full queue, the budget counts a GC copy of a parked item (the park
// would add it) and refuses the update before it integrates client 5's "k".
// V1 only: the V1 wire puts client 2 first, so the budget runs first.
func TestUnit_PendingBudget_AGCCopyOfAParkedItemCounts(t *testing.T) {
	room := New(WithClientID(1), WithMaxPendingItems(1))
	require.NoError(t, ApplyUpdateV1(room, structsV1(map[ClientID][]*Item{2: {waitingFor9(1)}}), nil))
	typist := New(WithClientID(5))
	typist.Transact(func(txn *Transaction) { txn.GetText("u").Insert(txn, 0, "k", nil) })
	update, err := MergeUpdatesV1(structsV1(map[ClientID][]*Item{2: {gcStruct(2, 1, 1)}}), EncodeStateAsUpdateV1(typist, nil))
	require.NoError(t, err)
	require.ErrorIs(t, ApplyUpdateV1(room, update, nil), ErrInvalidUpdate)
	require.Empty(t, room.GetText("u").ToString(), "refused by the budget, before any integration")
	require.Equal(t, 1, room.PendingStats().Items)
}

// The same, in the walk over collected groups: a three-deep client chain
// (3 on 4 on 5) makes both scans exceed the room, and client 2's group (a
// waiting item, then a GC copy of the parked clock 1) waits for client 9,
// which is on the wire but waits for the absent client 10. V1 only, as above.
func TestUnit_PendingBudget_AGCCopyInACollectedGroupCounts(t *testing.T) {
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

	room := New(WithClientID(1), WithMaxPendingItems(3))
	parked := &Item{ID: ID{Client: 2, Clock: 1}, Origin: &ID{Client: 9, Clock: 1}, Content: NewContentDeleted(1)}
	require.NoError(t, ApplyUpdateV1(room, structsV1(map[ClientID][]*Item{2: {parked}}), nil))
	require.ErrorIs(t, ApplyUpdateV1(room, update, nil), ErrInvalidUpdate)
	require.Empty(t, room.GetText("u").ToString(), "refused by the budget, before any integration")
	require.Equal(t, 1, room.PendingStats().Items)
}

// A park whose insertions would move more than coverInsertWork spans merges
// them in one pass per client; the cover then equals the one built from
// scratch, and the batch joins the span it touches.
func TestUnit_PendingCover_ABatchParkJoinsTheSpanItTouches(t *testing.T) {
	item := func(clock uint64, n int) *Item {
		return &Item{ID: ID{Client: 2, Clock: clock}, Content: NewContentDeleted(n)}
	}
	const spans = 10_000
	p := &pendingUpdate{missing: make(StateVector)}
	for i := range uint64(spans) {
		p.items = append(p.items, item(4*i+100, 1))
	}
	p.coverOf()
	const k = coverInsertWork/spans + 1 // more insertion work than the bound
	batch := make([]*Item, 0, k+1)
	for c := range uint64(k) {
		batch = append(batch, item(c, 1))
	}
	batch = append(batch, item(k, 100-k))
	p.items = append(p.items, batch...)
	p.addCover(batch)
	require.Equal(t, sortedSpans(p.items, false), p.cover.spans)
	require.Equal(t, clockSpan{0, 101}, p.cover.spans[2][0], "the batch joins the span at clock 100")
}

func TestUnit_PendingCover_Spans(t *testing.T) {
	item := func(clock uint64, n int) *Item {
		return &Item{ID: ID{Client: 1, Clock: clock}, Content: NewContentDeleted(n)}
	}
	p := &pendingUpdate{missing: make(StateVector)}
	add := func(it *Item) {
		p.items = append(p.items, it)
		p.addCover([]*Item{it})
	}
	for _, it := range []*Item{item(10, 2), item(4, 2), item(12, 3), item(6, 1), item(20, 1)} {
		add(it)
	}
	require.Equal(t, []clockSpan{{4, 7}, {10, 15}, {20, 21}}, p.coverOf().spans[1])
	for _, tc := range []struct{ start, end, want uint64 }{
		{4, 7, 7}, {5, 6, 6}, {4, 9, 7}, {3, 5, 3}, {10, 15, 15}, {14, 16, 15}, {15, 16, 15}, {7, 10, 7}, {20, 21, 21}, {8, 8, 8},
	} {
		require.Equal(t, tc.want, p.heldUntil(1, tc.start, 0, tc.end, false), "[%d,%d)", tc.start, tc.end)
	}
	require.Equal(t, uint64(7), p.heldUntil(1, 4, 6, 9, false), "from above clock")
	require.Equal(t, uint64(4), p.heldUntil(2, 4, 0, 7, false), "another client")
	require.True(t, p.holds(1, 8, 0, 8, false), "an empty range")
	require.Equal(t, uint64(4), (*pendingUpdate)(nil).heldUntil(1, 4, 0, 7, false), "no queue")
	require.Equal(t, uint64(10), p.heldTailFrom(1, 8, 15, false), "the span [10,15) holds the end")
	require.Equal(t, uint64(9), p.heldTailFrom(1, 8, 9, false), "no span holds 8")
	require.Equal(t, uint64(15), p.heldTailFrom(1, 10, 15, false), "the span holds the start too")
	// add keeps the cover in order and out of order, merging what touches.
	add(item(30, 1))
	add(item(25, 1))
	add(item(7, 3))
	require.Equal(t, []clockSpan{{4, 15}, {20, 21}, {25, 26}, {30, 31}}, p.cover.spans[1])
	require.Equal(t, sortedSpans(p.items, false), p.cover.spans)
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
		require.Equal(t, uint64(4), room.store.pending.items[1].ID.Clock, "v2=%v", v2)
		require.NoError(t, applyVersioned(t, room, v2, ab))
		require.Zero(t, room.PendingStats().Items)
		require.Equal(t, writer.GetText("t").ToString(), room.GetText("t").ToString(), "v2=%v", v2)
		require.Equal(t, EncodeStateAsUpdateV1(writer, nil), EncodeStateAsUpdateV1(room, nil), "v2=%v", v2)
	}
}
