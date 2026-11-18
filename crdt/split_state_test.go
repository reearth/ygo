package crdt

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// SplitStateAsUpdateV1 sends what SplitUpdateV1 sends for the encoded state,
// byte for byte, in every room below and to every receiver: reading a
// document's own structs changes what the split costs, never what it sends.
func TestUnit_SplitStateAsUpdateV1_MatchesTheSplitOfTheEncodedState(t *testing.T) {
	for _, room := range splitRooms(t) {
		t.Run(room.name, func(t *testing.T) {
			for _, sv := range append(receiverStates(room.doc), room.receivers...) {
				for _, maxBytes := range []int{200, 1 << 10, 64<<10 - 6} {
					want, wantErr := SplitUpdateV1(EncodeStateAsUpdateV1(room.doc, sv), sv, maxBytes)
					got, err := SplitStateAsUpdateV1(room.doc, sv, maxBytes)
					require.Equal(t, wantErr, err, "sv %v, maxBytes %d", sv, maxBytes)
					requireSameSeries(t, want, got, fmt.Sprintf("sv %v, maxBytes %d", sv, maxBytes))
				}
			}
		})
	}
}

// A receiver holding nothing that applies the series ends holding what a room
// holding parked structs or deletions holds: its clocks, its content, and the
// same clocks parked. Every update but the last integrates on its own,
// parking nothing.
func TestUnit_SplitStateAsUpdateV1_AReceiverEndsHoldingWhatAParkedRoomHolds(t *testing.T) {
	for _, room := range splitRooms(t) {
		if room.content == nil {
			continue
		}
		t.Run(room.name, func(t *testing.T) {
			for _, maxBytes := range []int{200, 1 << 10, 64<<10 - 6} {
				updates, err := SplitStateAsUpdateV1(room.doc, nil, maxBytes)
				require.NoError(t, err)
				got := New(WithClientID(99), WithMaxPendingItems(math.MaxInt))
				for i, u := range updates {
					require.NoError(t, ApplyUpdateV1(got, u, nil), "update %d of %d, maxBytes %d", i+1, len(updates), maxBytes)
					if stats := got.PendingStats(); i < len(updates)-1 && (stats.Items > 0 || stats.DeleteRanges > 0) {
						t.Fatalf("update %d of %d parks %+v, maxBytes %d", i+1, len(updates), stats, maxBytes)
					}
				}
				require.Equal(t, room.doc.StateVector(), got.StateVector(), "maxBytes %d", maxBytes)
				require.Equal(t, room.content(room.doc), room.content(got), "maxBytes %d", maxBytes)
				require.Equal(t, parkedClocks(room.doc), parkedClocks(got), "maxBytes %d", maxBytes)
			}
		})
	}
}

// parkedClocks is the clocks of each client doc holds parked, as structs past
// the clocks doc's own structs hold (parkedPastClocks) and as deletions, in
// ascending ranges.
func parkedClocks(doc *Doc) map[string][]DeleteRange {
	out := make(map[string][]DeleteRange)
	for c, ranges := range parkedPastClocks(doc) {
		out[fmt.Sprintf("struct %d", c)] = ranges
	}
	doc.mu.Lock()
	defer doc.mu.Unlock()
	for c, ranges := range doc.store.pendingDs.clients {
		out[fmt.Sprintf("deletion %d", c)] = slices.Clone(ranges)
	}
	return out
}

// SplitStateAsUpdateV1 merges a document's parked structs among themselves as
// withParked's merge does, but sorts them apart from the state's structs, so
// it could order two parked structs of one client on one clock otherwise than
// that merge does. The pending queue never holds two: it keeps each clock
// once, as Yjs merges its pending structs. A room here takes, shuffled, some
// of four writers' updates, their states cut at random state vectors, and GC
// structs over their clocks, and after each one no two structs it holds
// parked share a clock.
func TestUnit_PendingQueue_HoldsEachClockOfAClientOnce(t *testing.T) {
	var parked, gcParked int
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, 11))
		writers := make([]*Doc, 4)
		var updates [][]byte
		for i := range writers {
			writers[i] = New(WithClientID(ClientID(100 + i)))
			writers[i].OnUpdate(func(u []byte, _ any) { updates = append(updates, slices.Clone(u)) })
		}
		for range 80 {
			w := writers[rng.IntN(len(writers))]
			txt := w.GetText("t")
			w.Transact(func(txn *Transaction) {
				if txt.Len() > 2 && rng.IntN(4) == 0 {
					at := rng.IntN(txt.Len() - 1)
					txt.Delete(txn, at, 1+rng.IntN(min(3, txt.Len()-at)))
					return
				}
				txt.Insert(txn, rng.IntN(txt.Len()+1), strings.Repeat(string(rune('a'+rng.IntN(26))), 1+rng.IntN(4)), nil)
			})
			if rng.IntN(3) == 0 {
				a, b := writers[rng.IntN(len(writers))], writers[rng.IntN(len(writers))]
				if a != b {
					require.NoError(t, ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, b.StateVector()), nil))
				}
			}
			if rng.IntN(6) == 0 {
				a := writers[rng.IntN(len(writers))]
				cut := StateVector{}
				for c, clock := range a.StateVector() {
					cut[c] = uint64(rng.IntN(int(clock) + 1))
				}
				updates = append(updates, EncodeStateAsUpdateV1(a, cut))
			}
			if rng.IntN(8) == 0 {
				c := ClientID(100 + rng.IntN(len(writers)))
				clock := uint64(rng.IntN(int(writers[c-100].StateVector()[c]) + 1))
				updates = append(updates, encoding.EncodeBytes(func(enc *encoding.Encoder) {
					enc.WriteVarUint(1) // one struct group
					enc.WriteVarUint(1) // of one struct
					enc.WriteVarUint(uint64(c))
					enc.WriteVarUint(clock)
					enc.WriteUint8(0) // a GC struct
					enc.WriteVarUint(uint64(1 + rng.IntN(6)))
					enc.WriteVarUint(0) // no deletions
				}))
			}
		}
		rng.Shuffle(len(updates), func(i, j int) { updates[i], updates[j] = updates[j], updates[i] })
		room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
		for i, u := range updates {
			if rng.IntN(5) == 0 {
				continue // withheld, so what follows it parks
			}
			_ = ApplyUpdateV1(room, u, nil) // a GC struct can be refused; the queue is checked all the same
			room.mu.Lock()
			var held []*Item
			if room.store.pending != nil {
				held = room.store.pending.heldItems()
			}
			byClient := map[ClientID][]*Item{}
			for _, it := range held {
				byClient[it.ID.Client] = append(byClient[it.ID.Client], it)
				parked++
				if isGCStruct(it) {
					gcParked++
				}
			}
			room.mu.Unlock()
			for c, items := range byClient {
				slices.SortFunc(items, func(a, b *Item) int { return cmp.Compare(a.ID.Clock, b.ID.Clock) })
				for k := 1; k < len(items); k++ {
					prev := items[k-1]
					require.LessOrEqual(t, prev.ID.Clock+prev.Content.Len(), items[k].ID.Clock,
						"seed %d, update %d: client %d's parked structs at clocks %d and %d share a clock", seed, i, c, prev.ID.Clock, items[k].ID.Clock)
				}
			}
		}
	}
	require.Greater(t, parked, 1000, "the rooms park structs")
	require.Greater(t, gcParked, 10, "the rooms park GC structs")
	t.Logf("%d parked structs checked, %d of them GC structs", parked, gcParked)
}

// requireSameSeries fails at the first update of got that differs from want,
// naming the byte where it does.
func requireSameSeries(t *testing.T, want, got [][]byte, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d updates in the series, want %d; %s", len(got), len(want), what)
	}
	for i := range want {
		if bytes.Equal(want[i], got[i]) {
			continue
		}
		at := 0
		for at < len(want[i]) && at < len(got[i]) && want[i][at] == got[i][at] {
			at++
		}
		t.Fatalf("update %d of %d differs at byte %d: %d bytes, want %d; %s",
			i+1, len(want), at, len(got[i]), len(want[i]), what)
	}
}

// receiverStates are the state vectors of receivers doc is split for: one
// holding nothing, one holding half of each client's clocks, which falls
// inside a struct wherever a struct spans it, one holding every other client
// whole, one holding more than doc of every client, and one holding only a
// client doc never had.
func receiverStates(doc *Doc) []StateVector {
	full := doc.StateVector()
	clients := make([]ClientID, 0, len(full))
	for c := range full {
		clients = append(clients, c)
	}
	slices.SortFunc(clients, cmp.Compare)
	half, mixed, ahead := StateVector{}, StateVector{}, StateVector{}
	for i, c := range clients {
		half[c] = full[c] / 2
		if i%2 == 0 {
			mixed[c] = full[c]
		}
		ahead[c] = full[c] + 3
	}
	return []StateVector{nil, half, mixed, ahead, {999: 5}}
}

type splitRoom struct {
	name string
	doc  *Doc
	// receivers are state vectors the room is split for besides
	// receiverStates: one holding what the room's parked structs wait for.
	receivers []StateVector
	// content is what a receiver of a room holding parked structs or
	// deletions renders, or nil for a room holding neither.
	content func(*Doc) string
}

// splitRooms are documents holding every kind of struct the split reads
// otherwise than the decode of an encoded state would hand it: a struct cut at
// a receiver's clock, a keyed struct with an origin, whose parentSub the
// decode drops, a value the decode reads back as another type, a struct
// longer than an update, parked structs and deletions, and garbage-collected
// content. Each asserts that it holds what it is there for.
func splitRooms(t *testing.T) []splitRoom {
	t.Helper()
	typed := func(deleteEvery int) *Doc {
		doc := New(WithClientID(3_000_000_000))
		txt := doc.GetText("t")
		for i := range 5000 {
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, i/2, "a", nil) })
		}
		for at := 0; deleteEvery > 0 && at+5 < txt.Len(); at += deleteEvery {
			doc.Transact(func(txn *Transaction) { txt.Delete(txn, at, 5) })
		}
		return doc
	}
	text := func(doc *Doc) string { return doc.GetText("t").ToString() }
	parked, waitedFor := parkedRoom(t)
	gap, gapFilled := gapParkedRoom(t)
	keyed, keyedFilled := keyedParkedRoom(t)
	// Structs parked over clocks the room holds (parkedOverHeldClocks), which
	// wait on client 77's first clock.
	over, _ := parkedOverHeldClocks(t, 5, "ABCDEFG", DeleteRange{Clock: 6, Len: 4}, false)
	inside, _ := parkedOverHeldClocks(t, 4, "WXYZ", DeleteRange{Clock: 5, Len: 2}, false)
	same, _ := parkedOverHeldClocks(t, 6, "WXYZ", DeleteRange{Clock: 6, Len: 4}, false)
	behind, _ := parkedOverHeldClocks(t, 4, "WXYZ", DeleteRange{Clock: 5, Len: 2}, true)
	rightOrigin := StateVector{77: 1}
	return []splitRoom{
		{name: "characters typed into the middle", doc: typed(0)},
		{name: "characters typed into the middle, some deleted", doc: typed(37)},
		{name: "clients writing into one another's text", doc: interleavedRoom(t)},
		{name: "a struct whose origin is another client's", doc: crossOriginRoom(t)},
		{name: "structs longer than an update", doc: longStructRoom(t)},
		{name: "maps, nested types and XML", doc: keyedRoom(t)},
		{name: "values read back as other types", doc: rereadRoom(t)},
		{name: "parked structs and deletions", doc: parked, receivers: []StateVector{waitedFor}, content: text},
		{name: "structs and deletions parked behind a clock gap", doc: gap, receivers: []StateVector{gapFilled}, content: text},
		{name: "parked map entries and a parked nested map", doc: keyed, receivers: []StateVector{keyedFilled}, content: func(doc *Doc) string {
			out, err := doc.GetMap("m").ToJSON()
			require.NoError(t, err)
			return string(out)
		}},
		{name: "a struct parked over clocks the room holds", doc: over, receivers: []StateVector{rightOrigin}, content: text},
		{name: "a struct parked over part of a struct the room holds", doc: inside, receivers: []StateVector{rightOrigin}, content: text},
		{name: "a struct parked on the clock and length of a struct the room holds", doc: same, receivers: []StateVector{rightOrigin}, content: text},
		{name: "a struct parked behind a gap the room then filled", doc: behind, receivers: []StateVector{rightOrigin}, content: text},
		{name: "collected content", doc: collectedRoom(t)},
	}
}

// interleavedRoom is four clients' text, each round of each client written
// where the others' are, with some of it deleted.
func interleavedRoom(t *testing.T) *Doc {
	docs := make([]*Doc, 4)
	for i := range docs {
		docs[i] = New(WithClientID(ClientID(1000 + i)))
	}
	for round := range 30 {
		for i, d := range docs {
			txt := d.GetText("t")
			d.Transact(func(txn *Transaction) {
				at := 0
				if n := txt.Len(); n > 0 {
					at = (round*7 + i) % n
				}
				txt.Insert(txn, at, strings.Repeat(string(rune('a'+i)), 9), nil)
				if round%5 == 4 {
					txt.Delete(txn, at/2, 4)
				}
			})
		}
		for i, src := range docs {
			u := EncodeStateAsUpdateV1(src, nil)
			for j, dst := range docs {
				if i != j {
					require.NoError(t, ApplyUpdateV1(dst, u, nil))
				}
			}
		}
	}
	require.Len(t, docs[0].StateVector(), len(docs))
	return docs[0]
}

// crossOriginRoom holds a struct of one client whose origin is the other
// client's last clock. Cut at a receiver's clock, it names the clock before
// the cut instead, so a receiver holding half of each client's clocks can
// take it before the other client's rest.
func crossOriginRoom(t *testing.T) *Doc {
	a, doc := New(WithClientID(50)), New(WithClientID(60))
	aText, text := a.GetText("t"), doc.GetText("t")
	a.Transact(func(txn *Transaction) { aText.Insert(txn, 0, strings.Repeat("a", 10), nil) })
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(a, nil), nil))
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 10, strings.Repeat("b", 400), nil) })
	own := doc.store.clients[60]
	require.Len(t, own, 1)
	require.Equal(t, &ID{Client: 50, Clock: 9}, own[0].Origin, "the struct's origin is the other client's")
	return doc
}

// longStructRoom holds a text struct longer than any update, one of
// supplementary characters, which is cut only between characters, and a map
// value longer than any update, which is not cut at all.
func longStructRoom(t *testing.T) *Doc {
	doc := New(WithClientID(7))
	txt, emoji, m := doc.GetText("t"), doc.GetText("e"), doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, strings.Repeat("z", 200_000), nil)
		emoji.Insert(txn, 0, strings.Repeat("😀", 40_000), nil)
		m.Set(txn, "big", strings.Repeat("b", 100_000))
	})
	// The text a receiver holding half of it already has a part of: written
	// right after it, the next run joins the same struct.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 200_000, "more", nil) })
	longest := uint64(0)
	for _, it := range doc.store.clients[7] {
		longest = max(longest, it.Content.Len())
	}
	require.Greater(t, longest, uint64(64<<10), "the room holds a struct longer than an update")
	return doc
}

// keyedRoom holds map entries written over, whose structs name an origin and
// a key, nested maps and arrays whose structs name their container, and XML
// elements with attributes written over and text inside. Its last struct is a
// map entry of three clocks with no origin, written by hand, which a cut
// leaves keyed on both sides.
func keyedRoom(t *testing.T) *Doc {
	doc := New(WithClientID(11))
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		for i := range 300 {
			m.Set(txn, fmt.Sprintf("k%d", i%40), fmt.Sprintf("v%d", i))
		}
		inner := NewMapPrelim()
		m.Set(txn, "inner", inner)
		inner.Set(txn, "x", strings.Repeat("y", 50))
		inner.Set(txn, "x", strings.Repeat("z", 50))
		arr := NewArrayPrelim()
		m.Set(txn, "arr", arr)
		arr.Push(txn, []any{1, "two", 3.5, strings.Repeat("w", 300)})
	})
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "k3") })
	frag := doc.GetXmlFragment("x")
	doc.Transact(func(txn *Transaction) {
		for i := range 20 {
			p := NewYXmlElement("p")
			frag.InsertElement(txn, i, p)
			p.SetAttribute(txn, "class", "c")
			p.SetAttribute(txn, "class", fmt.Sprintf("c%d", i))
			p.SetAttributeValue(txn, "level", i)
			text := NewYXmlText()
			p.InsertText(txn, 0, text)
			text.Insert(txn, 0, strings.Repeat("t", 30), nil)
		}
	})
	keyed := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1)  // one struct group
		enc.WriteVarUint(1)  // of one struct
		enc.WriteVarUint(23) // client
		enc.WriteVarUint(0)  // clock
		enc.WriteUint8(wireAny | flagHasParentSub)
		enc.WriteUint8(1) // a root type, by name
		enc.WriteVarString("keyed")
		enc.WriteVarString("k")
		enc.WriteVarUint(3)
		for _, v := range []string{"p", "q", "r"} {
			enc.WriteAny(strings.Repeat(v, 300))
		}
		enc.WriteVarUint(0) // no deletions
	})
	require.NoError(t, ApplyUpdateV1(doc, keyed, nil))

	var withOrigin, cutKeyed bool
	for _, items := range doc.store.clients {
		for _, it := range items {
			withOrigin = withOrigin || it.ParentSub != nil && it.Origin != nil
			cutKeyed = cutKeyed || it.ParentSub != nil && it.Origin == nil && it.Content.Len() > 1
		}
	}
	require.True(t, withOrigin, "the room holds a keyed struct with an origin")
	require.True(t, cutKeyed, "the room holds a keyed struct an update can cut")
	return doc
}

// rereadRoom holds values written through the Go API that the decode reads
// back as other types, which are written in other bytes: an integer past
// int32 that a float32 holds, a JSON number past float64's integers, and a
// subdocument, whose options the decode rebuilds.
func rereadRoom(t *testing.T) *Doc {
	doc := New(WithClientID(13))
	m := doc.GetMap("v")
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "int40", int64(1)<<40)
		m.Set(txn, "uint", uint64(1)<<33)
		m.Set(txn, "int", 7)
		m.Set(txn, "nested", map[string]any{"a": []any{int64(1) << 41, "s", nil, true}, "b": float32(1.5)})
		m.Set(txn, "bytes", []byte{1, 2, 3})
		m.Set(txn, "bigint", encoding.BigInt(1<<60))
		m.Set(txn, "sub", New(WithGUID("subdocument"), WithClientID(14)))
		txt.Insert(txn, 0, "formatted text", nil)
		txt.Format(txn, 0, 9, Attributes{"bold": true, "size": int64(1)<<60 + 1})
		txt.InsertEmbed(txn, 4, map[string]any{"src": "x.png", "width": 640}, nil)
	})
	reread := 0
	for _, items := range doc.store.clients {
		for _, it := range items {
			if !rereadsSame(it.Content) {
				reread++
			}
		}
	}
	require.GreaterOrEqual(t, reread, 4, "structs the decode reads back as other values")
	return doc
}

// parkedRoom holds structs that name a client it does not hold, and
// deletions of that client's clocks, both parked. It returns the state vector
// of a receiver holding that client's clocks, for which none of them waits.
func parkedRoom(t *testing.T) (*Doc, StateVector) {
	a, b, c := New(WithClientID(31)), New(WithClientID(32)), New(WithClientID(33))
	aText, bText, cText := a.GetText("t"), b.GetText("t"), c.GetText("t")
	a.Transact(func(txn *Transaction) { aText.Insert(txn, 0, strings.Repeat("a", 400), nil) })
	require.NoError(t, ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, nil), nil))
	b.Transact(func(txn *Transaction) { bText.Insert(txn, 200, strings.Repeat("b", 300), nil) })
	require.NoError(t, ApplyUpdateV1(c, EncodeStateAsUpdateV1(b, nil), nil))
	c.Transact(func(txn *Transaction) {
		cText.Insert(txn, 350, strings.Repeat("c", 300), nil)
		cText.Delete(txn, 250, 20)
	})

	doc := New(WithClientID(34))
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(a, nil), nil))
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, strings.Repeat("d", 100), nil) })
	require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(c, b.StateVector()), nil))
	stats := doc.PendingStats()
	require.NotZero(t, stats.Items, "the room parks structs")
	require.NotZero(t, stats.DeleteRanges, "the room parks deletions")
	return doc, StateVector{32: b.StateVector()[32]}
}

// gapParkedRoom holds structs parked behind a gap in their own client's
// clocks: runs of a text of which the room missed one, and a deletion across
// the gap, which deletes what the room holds and parks the rest. It also
// holds a GC struct of a client it holds nothing of, written from clock 1,
// and text of its own. It returns the state vector of a receiver holding the
// clocks the room misses.
func gapParkedRoom(t *testing.T) (*Doc, StateVector) {
	writer := New(WithClientID(51))
	written := writer.GetText("t")
	var updates [][]byte
	writer.OnUpdate(func(u []byte, _ any) { updates = append(updates, slices.Clone(u)) })
	for i := range 5 {
		writer.Transact(func(txn *Transaction) {
			written.Insert(txn, written.Len(), strings.Repeat(string(rune('a'+i)), 100), nil)
		})
	}
	writer.Transact(func(txn *Transaction) { written.Delete(txn, 150, 100) })

	doc := New(WithClientID(52))
	for _, i := range []int{0, 1, 3, 4, 5} { // not run 2, clocks 200 to 299
		require.NoError(t, ApplyUpdateV1(doc, updates[i], nil))
	}
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, strings.Repeat("r", 50), nil) })
	gc := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1)  // one struct group
		enc.WriteVarUint(1)  // of one struct
		enc.WriteVarUint(53) // client
		enc.WriteVarUint(1)  // clock
		enc.WriteUint8(0)    // a GC struct
		enc.WriteVarUint(1)
		enc.WriteVarUint(0) // no deletions
	})
	require.NoError(t, ApplyUpdateV1(doc, gc, nil))
	stats := doc.PendingStats()
	require.Equal(t, 3, stats.Items, "the room parks runs 3 and 4 and the GC struct")
	require.NotZero(t, stats.DeleteRanges, "the room parks deletions")
	require.Equal(t, uint64(200), doc.StateVector()[51])
	return doc, StateVector{51: 300, 53: 1}
}

// keyedParkedRoom holds map entries parked behind a gap in their client's
// clocks: entries of a nested map, written as children of a container the
// room does not hold, each with its key, and an entry written over, which
// names the entry before it as its origin. It returns the state vector of a
// receiver holding the container.
func keyedParkedRoom(t *testing.T) (*Doc, StateVector) {
	writer := New(WithClientID(61))
	m := writer.GetMap("m")
	var updates [][]byte
	writer.OnUpdate(func(u []byte, _ any) { updates = append(updates, slices.Clone(u)) })
	writer.Transact(func(txn *Transaction) { m.Set(txn, "inner", NewMapPrelim()) })
	got, ok := m.Get("inner")
	require.True(t, ok)
	inner := got.(*YMap)
	writer.Transact(func(txn *Transaction) {
		inner.Set(txn, "a", "x")
		inner.Set(txn, "b", strings.Repeat("y", 300))
	})
	writer.Transact(func(txn *Transaction) { m.Set(txn, "k", 1) })
	writer.Transact(func(txn *Transaction) { m.Set(txn, "k", 2) })

	doc := New(WithClientID(62))
	own := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		for i := range 40 {
			own.Set(txn, fmt.Sprintf("own%d", i), strings.Repeat("o", 20))
		}
	})
	require.NoError(t, ApplyUpdateV1(doc, updates[1], nil))
	require.NoError(t, ApplyUpdateV1(doc, updates[3], nil))
	require.Equal(t, 3, doc.PendingStats().Items, "the room parks both nested entries and the entry written over")
	var keyed, origin bool
	for _, it := range doc.store.pending.heldItems() {
		keyed = keyed || it.ParentSub != nil && it.Origin == nil
		origin = origin || it.Origin != nil
	}
	require.True(t, keyed && origin, "the room parks a keyed struct and a struct with an origin")
	return doc, StateVector{61: 1}
}

// collectedRoom holds deleted text and a deleted nested map, whose content
// garbage collection has dropped.
func collectedRoom(t *testing.T) *Doc {
	doc := New(WithClientID(41))
	m := doc.GetMap("m")
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		inner := NewMapPrelim()
		m.Set(txn, "inner", inner)
		for i := range 30 {
			inner.Set(txn, fmt.Sprintf("k%d", i), strings.Repeat("v", 40))
		}
		txt.Insert(txn, 0, strings.Repeat("x", 3000), nil)
	})
	doc.Transact(func(txn *Transaction) {
		m.Delete(txn, "inner")
		txt.Delete(txn, 100, 2000)
	})
	collected := false
	for _, it := range doc.store.clients[41] {
		_, gone := it.Content.(*ContentDeleted)
		collected = collected || gone
	}
	require.True(t, collected, "the room holds collected content")
	return doc
}
