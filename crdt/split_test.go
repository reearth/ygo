package crdt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// applySeries applies updates in order to a fresh document, asserting after
// each one that nothing is left parked: the property a split exists for, since
// a struct waiting for an update still to come is a struct the receiver cannot
// render, and asserting that every update but the last is within maxBytes.
func applySeries(t *testing.T, updates [][]byte, maxBytes int) *Doc {
	t.Helper()
	got := New(WithClientID(99))
	for i, u := range updates {
		require.NoError(t, ApplyUpdateV1(got, u, nil), "update %d of %d applies", i+1, len(updates))
		stats := got.PendingStats()
		assert.Zero(t, stats.Items, "structs parked after update %d of %d", i+1, len(updates))
		assert.Zero(t, stats.DeleteRanges, "delete ranges parked after update %d of %d", i+1, len(updates))
		if i < len(updates)-1 {
			assert.LessOrEqual(t, len(u), maxBytes, "bytes of update %d of %d", i+1, len(updates))
		}
	}
	return got
}

// A split carries a document whole, in updates that each integrate alone.
func TestUnit_SplitUpdateV1_RebuildsTheDocumentFromBoundedUpdates(t *testing.T) {
	const maxBytes = 1 << 10
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	for range 40 {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, strings.Repeat("abcde", 100), nil) })
	}
	state := EncodeStateAsUpdateV1(doc, nil)
	require.Greater(t, len(state), 16*maxBytes, "the fixture needs several updates")

	updates, err := SplitUpdateV1(state, nil, maxBytes)
	require.NoError(t, err)
	require.Greater(t, len(updates), 16, "updates the state was cut into")

	got := applySeries(t, updates, maxBytes)
	assert.Equal(t, txt.ToString(), got.GetText("t").ToString())
	assert.Equal(t, doc.StateVector(), got.StateVector())
}

// Every deletion rides with the struct it names, so no update leaves a
// receiver holding content a later one deletes.
func TestUnit_SplitUpdateV1_DeletionsRideWithTheStructsTheyName(t *testing.T) {
	const maxBytes = 512
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	// Every tenth run is marked, and only the marked runs are deleted, so a
	// receiver holding one is holding content the document does not.
	for i := range 40 {
		mark := "keep"
		if i%10 == 9 {
			mark = "GONE"
		}
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, strings.Repeat(mark, 25), nil) })
	}
	for {
		at := strings.Index(txt.ToString(), "GONE")
		if at < 0 {
			break
		}
		doc.Transact(func(txn *Transaction) { txt.Delete(txn, at, 4) })
	}
	want := txt.ToString()
	require.NotContains(t, want, "GONE")

	updates, err := SplitUpdateV1(EncodeStateAsUpdateV1(doc, nil), nil, maxBytes)
	require.NoError(t, err)
	require.Greater(t, len(updates), 1)

	got := New(WithClientID(99))
	for i, u := range updates {
		require.NoError(t, ApplyUpdateV1(got, u, nil))
		assert.NotContains(t, got.GetText("t").ToString(), "GONE",
			"after update %d of %d the receiver holds deleted content", i+1, len(updates))
	}
	assert.Equal(t, want, got.GetText("t").ToString())
}

// A text item longer than one update is cut into several, each integrating on
// its own, which a split by byte length could not do.
func TestUnit_SplitUpdateV1_CutsOneItemLongerThanAnUpdate(t *testing.T) {
	const maxBytes = 1 << 12
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	// One run of text written in one transaction is one struct in the store.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, strings.Repeat("z", 100_000), nil) })
	require.Len(t, doc.store.clients[1], 1, "the fixture is one struct")

	updates, err := SplitUpdateV1(EncodeStateAsUpdateV1(doc, nil), nil, maxBytes)
	require.NoError(t, err)
	require.Greater(t, len(updates), 20, "the one struct is cut into updates")

	got := applySeries(t, updates, maxBytes)
	assert.Equal(t, txt.ToString(), got.GetText("t").ToString())
}

// A cut never bisects a supplementary character: Splice would leave a
// replacement character on either side of it, changing the text it carries.
func TestUnit_SplitUpdateV1_KeepsSupplementaryCharactersWhole(t *testing.T) {
	const maxBytes = 64
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, strings.Repeat("😀", 400), nil) })
	want := txt.ToString()

	updates, err := SplitUpdateV1(EncodeStateAsUpdateV1(doc, nil), nil, maxBytes)
	require.NoError(t, err)
	require.Greater(t, len(updates), 10, "the fixture is cut into updates")

	got := applySeries(t, updates, maxBytes)
	assert.Equal(t, want, got.GetText("t").ToString())
	assert.NotContains(t, got.GetText("t").ToString(), "\uFFFD", "no character was bisected")
}

// Structs of several clients, each naming the others, go out in an order that
// leaves none waiting.
func TestUnit_SplitUpdateV1_OrdersInterleavedClients(t *testing.T) {
	const maxBytes = 256
	docs := make([]*Doc, 4)
	for i := range docs {
		docs[i] = New(WithClientID(ClientID(i + 1)))
	}
	for round := range 30 {
		for i, d := range docs {
			txt := d.GetText("t")
			at := 0
			if n := txt.Len(); n > 0 {
				at = (round*7 + i) % n
			}
			d.Transact(func(txn *Transaction) {
				txt.Insert(txn, at, strings.Repeat(string(rune('a'+i)), 9), nil)
			})
		}
		// Every document hears every other one's round before the next.
		for i, src := range docs {
			u := EncodeStateAsUpdateV1(src, nil)
			for j, dst := range docs {
				if i != j {
					require.NoError(t, ApplyUpdateV1(dst, u, nil))
				}
			}
		}
	}
	room := docs[0]
	require.Len(t, room.StateVector(), len(docs), "the fixture has every client")
	want := room.GetText("t").ToString()

	updates, err := SplitUpdateV1(EncodeStateAsUpdateV1(room, nil), nil, maxBytes)
	require.NoError(t, err)
	require.Greater(t, len(updates), 5)

	got := applySeries(t, updates, maxBytes)
	assert.Equal(t, want, got.GetText("t").ToString())
	assert.Equal(t, room.StateVector(), got.StateVector())
}

// A receiver that already holds part of the document is sent only the rest,
// cut at the clocks it holds, as the one update it replaces would have been.
func TestUnit_SplitUpdateV1_SendsOnlyWhatAReceiverLacks(t *testing.T) {
	const maxBytes = 1 << 10
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, strings.Repeat("a", 3000), nil) })

	got := New(WithClientID(99))
	require.NoError(t, ApplyUpdateV1(got, EncodeStateAsUpdateV1(doc, nil), nil))
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 3000, strings.Repeat("b", 5000), nil) })

	sv := got.StateVector()
	updates, err := SplitUpdateV1(EncodeStateAsUpdateV1(doc, sv), sv, maxBytes)
	require.NoError(t, err)
	for i, u := range updates {
		require.NoError(t, ApplyUpdateV1(got, u, nil), "update %d applies", i+1)
		assert.Zero(t, got.PendingStats().Items, "structs parked after update %d", i+1)
	}
	assert.Equal(t, txt.ToString(), got.GetText("t").ToString())
}

// A structure of nested types splits into updates that each integrate alone:
// a child never arrives before the container item it names.
func TestUnit_SplitUpdateV1_KeepsNestedTypesBehindTheirContainers(t *testing.T) {
	const maxBytes = 256
	doc := New(WithClientID(1))
	root := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		for i := range 30 {
			inner := NewMapPrelim()
			root.Set(txn, "k"+strings.Repeat("x", i%5)+string(rune('a'+i)), inner)
			inner.Set(txn, "text", strings.Repeat("v", 40))
		}
	})
	want, err := doc.GetMap("m").ToJSON()
	require.NoError(t, err)

	updates, err := SplitUpdateV1(EncodeStateAsUpdateV1(doc, nil), nil, maxBytes)
	require.NoError(t, err)
	require.Greater(t, len(updates), 5)

	got := applySeries(t, updates, maxBytes)
	gotJSON, err := got.GetMap("m").ToJSON()
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(gotJSON))
}

// A document with nothing in it still yields the one update a caller sends as
// the message that ends a reply.
func TestUnit_SplitUpdateV1_EmptyStateIsOneUpdate(t *testing.T) {
	updates, err := SplitUpdateV1(EncodeStateAsUpdateV1(New(), nil), nil, 1<<16)
	require.NoError(t, err)
	require.Len(t, updates, 1)
	assert.NoError(t, ApplyUpdateV1(New(), updates[0], nil))
}

// A struct a V1 update cannot cut is carried whole, over the limit, rather
// than dropped or cut into content the receiver cannot read.
func TestUnit_SplitUpdateV1_CarriesAnUncuttableStructWhole(t *testing.T) {
	const maxBytes = 1 << 10
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Insert(txn, 0, []any{strings.Repeat("b", 8<<10)}) })
	require.Len(t, doc.store.clients[1], 1, "the fixture is one struct")
	require.Equal(t, uint64(1), doc.store.clients[1][0].Content.Len(), "of one clock, so it cannot be cut")

	updates, err := SplitUpdateV1(EncodeStateAsUpdateV1(doc, nil), nil, maxBytes)
	require.NoError(t, err)
	got := New(WithClientID(99))
	for _, u := range updates {
		require.NoError(t, ApplyUpdateV1(got, u, nil))
		assert.Zero(t, got.PendingStats().Items)
	}
	want, err := doc.GetArray("a").ToJSON()
	require.NoError(t, err)
	gotJSON, err := got.GetArray("a").ToJSON()
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(gotJSON))
}

// varUintLen agrees with what the encoder writes.
func TestUnit_VarUintLen_MatchesTheEncoder(t *testing.T) {
	for _, v := range []uint64{0, 1, 0x7F, 0x80, 0x3FFF, 0x4000, 1 << 35, ^uint64(0)} {
		written := encoding.EncodeBytes(func(enc *encoding.Encoder) { enc.WriteVarUint(v) })
		assert.Len(t, written, varUintLen(v), "VarUint(%d)", v)
	}
}

// A span and a delete range wider than a 32-bit length travel whole, and the
// struct that follows the span still goes out behind it: every clock and
// length the splitter works in is uint64, so a 32-bit build reads the same
// widths as a 64-bit one.
func TestUnit_SplitUpdateV1_CarriesSpansWiderThan32Bits(t *testing.T) {
	const wide = uint64(1) << 33
	scratch := New(WithClientID(1))
	parent := scratch.GetText("t").baseType()
	span := &Item{ID: ID{Client: 1, Clock: 0}, Content: NewContentDeleted(wide), Deleted: true}
	after := &Item{
		ID:      ID{Client: 1, Clock: wide},
		Origin:  &ID{Client: 1, Clock: wide - 1},
		Parent:  parent,
		Content: NewContentString("a"),
	}
	ds := newDeleteSet()
	ds.clients[1] = []DeleteRange{{Clock: 0, Len: wide}}
	update := encodeStructStoreV1(map[ClientID][]*Item{1: {span, after}}, ds, StateVector{})

	// A budget too small for both structs at once puts each in an update of
	// its own, so the series has to order them.
	updates, err := SplitUpdateV1(update, nil, 24)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(updates), 3, "each struct goes in an update of its own")
	merged, err := MergeUpdatesV1(updates...)
	require.NoError(t, err)
	perClient, mergedDS, err := buildMergeStore([][]byte{merged}, decodeStructsV1)
	require.NoError(t, err)
	require.Len(t, perClient[1], 2, "both structs survive the series")
	assert.Equal(t, wide, perClient[1][0].Content.Len(), "the span keeps its width")
	assert.Equal(t, wide, perClient[1][1].ID.Clock, "the struct after it keeps its clock")
	assert.Equal(t, []DeleteRange{{Clock: 0, Len: wide}}, mergedDS.clients[1], "the delete range keeps its width")

	// The span's update comes before the one that names its last clock.
	spanAt, afterAt := -1, -1
	for i, u := range updates {
		per, _, err := buildMergeStore([][]byte{u}, decodeStructsV1)
		require.NoError(t, err)
		for _, it := range per[1] {
			switch it.ID.Clock {
			case 0:
				spanAt = i
			case wide:
				afterAt = i
			}
		}
	}
	require.NotEqual(t, -1, spanAt)
	require.NotEqual(t, -1, afterAt)
	assert.Less(t, spanAt, afterAt, "the span goes out before the struct that names it")
}
