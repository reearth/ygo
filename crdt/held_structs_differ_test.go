package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for HeldStructsDifferV1: whether an update carries, at a (client,
// clock) a document holds, a struct that differs from the document's.

// typed is the update of the transaction in which doc types text at index of
// root text "t".
func typed(doc *Doc, index int, text string) []byte {
	before := doc.StateVector()
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, index, text, nil) })
	return EncodeStateAsUpdateV1(doc, before)
}

// typedIn is typed into root text root.
func typedIn(doc *Doc, root, text string) []byte {
	before := doc.StateVector()
	txt := doc.GetText(root)
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, text, nil) })
	return EncodeStateAsUpdateV1(doc, before)
}

// setKey is the update of the transaction in which doc sets key of m to v.
func setKey(doc *Doc, m *YMap, key string, v any) []byte {
	before := doc.StateVector()
	doc.Transact(func(txn *Transaction) { m.Set(txn, key, v) })
	return EncodeStateAsUpdateV1(doc, before)
}

// holding returns a document of client 99 that applied updates.
func holding(t *testing.T, updates ...[]byte) *Doc {
	t.Helper()
	return clientHolding(t, 99, updates...)
}

// clientHolding returns a document of client that applied updates.
func clientHolding(t *testing.T, client uint64, updates ...[]byte) *Doc {
	t.Helper()
	doc := newTestDoc(client)
	for _, u := range updates {
		require.NoError(t, ApplyUpdateV1(doc, u, nil))
	}
	return doc
}

func differ(t *testing.T, doc *Doc, update []byte, client ClientID, from uint64) bool {
	t.Helper()
	d, err := HeldStructsDifferV1(doc, update, client, from)
	require.NoError(t, err)
	return d
}

// A resend of what the document holds does not differ, whole or in part, and
// however the document merged or split the structs it holds.
func TestUnit_HeldStructsDifferV1_AnEqualResendDoesNotDiffer(t *testing.T) {
	writer := newTestDoc(7)
	a, b, c := typed(writer, 0, "a"), typed(writer, 1, "b"), typed(writer, 2, "c")
	m := writer.GetMap("m")
	key := setKey(writer, m, "k", 1)
	again := setKey(writer, m, "k", "two")
	embed := func() []byte {
		before := writer.StateVector()
		txt := writer.GetText("t")
		writer.Transact(func(txn *Transaction) { txt.InsertEmbed(txn, 0, map[string]any{"img": "x"}, nil) })
		return EncodeStateAsUpdateV1(writer, before)
	}()
	room := holding(t, EncodeStateAsUpdateV1(writer, nil))
	midway := typed(writer, 2, "-") // splits the run "abc" in room once applied
	require.NoError(t, ApplyUpdateV1(room, midway, nil))

	for name, update := range map[string][]byte{
		"the whole state":   EncodeStateAsUpdateV1(writer, nil),
		"one keystroke":     b,
		"the last one":      c,
		"two merged":        mustMerge(t, a, b),
		"a map entry":       key,
		"its overwrite":     again,
		"an embed":          embed,
		"the split's own":   midway,
		"the state from b+": EncodeStateAsUpdateV1(writer, StateVector{7: 1}),
	} {
		assert.False(t, differ(t, room, update, 7, 0), name)
	}
}

// The same client and clock with another content unit, origin, right origin,
// parent or key differs.
func TestUnit_HeldStructsDifferV1_ASecondWriterAtAHeldClockDiffers(t *testing.T) {
	other := newTestDoc(9)
	q := typed(other, 0, "Q")
	creator := newTestDoc(1)
	root := creator.GetMap("m")
	creator.Transact(func(txn *Transaction) {
		root.Set(txn, "x", NewMapPrelim())
		root.Set(txn, "y", NewMapPrelim())
	})
	containers := EncodeStateAsUpdateV1(creator, nil)
	nestedIn := func(doc *Doc, name string) *YMap {
		v, ok := doc.GetMap("m").Get(name)
		require.True(t, ok)
		return v.(*YMap)
	}
	// withQ is a document of client 7 that holds client 9's "Q", and
	// withContainers one that holds client 1's two nested maps.
	withQ := func() *Doc { return clientHolding(t, 7, q) }
	withContainers := func() *Doc { return clientHolding(t, 7, containers) }
	for name, tc := range map[string]struct {
		first, second func() []byte
	}{
		"content unit": {
			first:  func() []byte { return typed(newTestDoc(7), 0, "abc") },
			second: func() []byte { return typed(newTestDoc(7), 0, "abx") },
		},
		"origin": {
			first: func() []byte { return typed(newTestDoc(7), 0, "a") },
			second: func() []byte {
				doc := withQ()
				return typed(doc, 1, "a")
			},
		},
		"right origin": {
			first: func() []byte { return typed(newTestDoc(7), 0, "a") },
			second: func() []byte {
				doc := withQ()
				return typed(doc, 0, "a")
			},
		},
		"root parent": {
			first:  func() []byte { return typedIn(newTestDoc(7), "t", "a") },
			second: func() []byte { return typedIn(newTestDoc(7), "u", "a") },
		},
		"nested parent": {
			first: func() []byte {
				doc := withContainers()
				return setKey(doc, nestedIn(doc, "x"), "k", 1)
			},
			second: func() []byte {
				doc := withContainers()
				return setKey(doc, nestedIn(doc, "y"), "k", 1)
			},
		},
		"key": {
			first: func() []byte {
				doc := newTestDoc(7)
				return setKey(doc, doc.GetMap("m"), "k1", 1)
			},
			second: func() []byte {
				doc := newTestDoc(7)
				return setKey(doc, doc.GetMap("m"), "k2", 1)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			first, second := tc.first(), tc.second()
			room := holding(t, q, containers, first)
			assert.False(t, differ(t, room, first, 7, 0), "the first writer's own update")
			assert.True(t, differ(t, room, second, 7, 0), "the second writer's")
		})
	}
}

// A struct the document does not hold is not compared: one past the
// document's clock, one parked in its pending queue, one of another client,
// and a clock before from.
func TestUnit_HeldStructsDifferV1_AStructTheDocDoesNotHoldIsNotCompared(t *testing.T) {
	first := newTestDoc(7)
	ab := typed(first, 0, "ab")
	typed(first, 2, "cde")
	late := typed(first, 5, "f")
	room := holding(t, ab, late)
	require.Equal(t, 1, room.PendingStats().Items, "the room parks the struct at clock 5")

	second := newTestDoc(7)
	typed(second, 0, "ab")
	typed(second, 2, "CDE")
	lateSecond := typed(second, 5, "F")
	assert.False(t, differ(t, room, EncodeStateAsUpdateV1(second, StateVector{7: 2}), 7, 0),
		"clocks 2 to 4 are past what the room holds")
	assert.False(t, differ(t, room, lateSecond, 7, 0), "clock 5 is parked, not held")
	assert.False(t, differ(t, room, EncodeStateAsUpdateV1(second, nil), 7, 0),
		"the units the room holds are equal")

	third := newTestDoc(7)
	xy := typed(third, 0, "xy")
	assert.True(t, differ(t, room, xy, 7, 0))
	assert.True(t, differ(t, room, xy, 7, 1), "clock 1 is compared from 1")
	assert.False(t, differ(t, room, xy, 7, 2), "no clock from 2 is held")
	assert.False(t, differ(t, room, xy, 8, 0), "client 8's structs only are compared")
	assert.False(t, differ(t, newTestDoc(99), xy, 7, 0), "a document holding nothing")
}

// Deleted content keeps its origins, which are compared, and not its
// content, which is not.
func TestUnit_HeldStructsDifferV1_DeletedContentIsComparedByItsOrigins(t *testing.T) {
	first := newTestDoc(7)
	abc := typed(first, 0, "abc")
	room := holding(t, abc)
	txt := room.GetText("t")
	room.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 3) })
	require.Empty(t, txt.ToString())

	assert.False(t, differ(t, room, abc, 7, 0), "the deleted run sent again")
	assert.False(t, differ(t, room, typed(newTestDoc(7), 0, "xyz"), 7, 0),
		"other content at the same clocks and origins: the room no longer holds the content")
	withQ := clientHolding(t, 7, typed(newTestDoc(9), 0, "Q"))
	assert.True(t, differ(t, room, typed(withQ, 1, "abc"), 7, 0), "another origin still differs")
}

// An update that does not decode returns an error and false, and no prefix
// of a valid update panics.
func TestUnit_HeldStructsDifferV1_AMalformedUpdateErrors(t *testing.T) {
	writer := newTestDoc(7)
	typed(writer, 0, "abc")
	m := writer.GetMap("m")
	setKey(writer, m, "k", map[string]any{"n": 1.5})
	update := EncodeStateAsUpdateV1(writer, nil)
	room := holding(t, update)

	for _, bad := range [][]byte{nil, {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, update[:len(update)/2]} {
		d, err := HeldStructsDifferV1(room, bad, 7, 0)
		require.ErrorIs(t, err, ErrInvalidUpdate, "%x", bad)
		assert.False(t, d)
	}
	for n := range len(update) {
		require.NotPanics(t, func() {
			if d, err := HeldStructsDifferV1(room, update[:n], 7, 0); err != nil {
				assert.False(t, d)
			}
		}, "the first %d bytes", n)
	}
}

func mustMerge(t *testing.T, updates ...[]byte) []byte {
	t.Helper()
	u, err := MergeUpdatesV1(updates...)
	require.NoError(t, err)
	return u
}
