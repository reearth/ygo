package crdt

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Doc.SetParkedOutOfState: what a document holds parked is in no
// encode of its state and in no count of it, and is still held, drained and
// integrated.

// outOfStateRoom is a document of client 99 holding client 7's "ab", and,
// unless control, client 7's "cd" parked behind a gap of client 7 and client
// 9's deletion of a character the room lacks parked too. fill is the update
// that fills the gap.
func outOfStateRoom(t *testing.T, on, control bool) (room *Doc, fill []byte) {
	t.Helper()
	writer := New(WithClientID(7))
	txt := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ab", nil) })
	ab := EncodeStateAsUpdateV1(writer, nil)
	sv := writer.StateVector()
	writer.Transact(func(txn *Transaction) { txt.Insert(txn, 2, "xy", nil) })
	fill = EncodeStateAsUpdateV1(writer, sv)
	sv = writer.StateVector()
	writer.Transact(func(txn *Transaction) { txt.Insert(txn, 4, "cd", nil) })
	cd := EncodeStateAsUpdateV1(writer, sv)

	deleter := New(WithClientID(9))
	require.NoError(t, ApplyUpdateV1(deleter, EncodeStateAsUpdateV1(writer, nil), nil))
	before := deleter.StateVector()
	dtxt := deleter.GetText("t")
	deleter.Transact(func(txn *Transaction) { dtxt.Delete(txn, 2, 1) }) // the "x"
	deletion := EncodeStateAsUpdateV1(deleter, before)

	room = New(WithClientID(99))
	room.SetParkedOutOfState(on)
	require.NoError(t, ApplyUpdateV1(room, ab, nil))
	if !control {
		require.NoError(t, ApplyUpdateV1(room, cd, nil))
		require.NoError(t, ApplyUpdateV1(room, deletion, nil))
		require.Equal(t, PendingStats{Items: 1, DeleteRanges: 1, ContentBytes: len("cd"), MissingFor: []ClientID{7}}, room.PendingStats())
	}
	return room, fill
}

// encodes is every encode of room's state the option governs: V1, V2, and the
// split a sync step 1 is answered with.
func encodes(t *testing.T, room *Doc) map[string][]byte {
	t.Helper()
	split, err := SplitStateAsUpdateV1(room, nil, 1<<20)
	require.NoError(t, err)
	return map[string][]byte{
		"EncodeStateAsUpdateV1": EncodeStateAsUpdateV1(room, nil),
		"EncodeStateAsUpdateV2": EncodeStateAsUpdateV2(room, nil),
		"SplitStateAsUpdateV1":  []byte(fmt.Sprintf("%x", split)),
	}
}

// On, every encode of the state is the state of a room that never received
// what it parks; off, each carries the parked struct and deletion.
func TestUnit_ParkedOutOfState_NoEncodeOfTheStateCarriesWhatIsParked(t *testing.T) {
	control, _ := outOfStateRoom(t, false, true)
	want := encodes(t, control)
	for name, on := range map[string]bool{"on": true, "off": false} {
		t.Run(name, func(t *testing.T) {
			room, _ := outOfStateRoom(t, on, false)
			assert.Equal(t, on, room.ParkedOutOfState())
			for form, got := range encodes(t, room) {
				assert.Equal(t, on, bytes.Equal(got, want[form]), "%s: equal to the state of a room that parked nothing", form)
			}
			assert.Equal(t, 1, room.PendingStats().Items, "the queue still holds what parked")
		})
	}
}

// What is parked is still held: the fill drains it, its commit carries it,
// and the state then holds it.
func TestUnit_ParkedOutOfState_TheFillStillLandsWhatIsParked(t *testing.T) {
	room, fill := outOfStateRoom(t, true, false)
	var landed []byte
	room.OnUpdate(func(u []byte, _ any) { landed = u })
	require.NoError(t, ApplyUpdateV1(room, fill, nil))
	assert.Equal(t, "abycd", room.GetText("t").ToString(), "the parked text and deletion land")
	assert.Equal(t, PendingStats{}, room.PendingStats())

	reader, _ := outOfStateRoom(t, false, true)
	require.NoError(t, ApplyUpdateV1(reader, landed, nil))
	assert.Equal(t, "abycd", reader.GetText("t").ToString(), "the fill's commit carries what its drain landed")
}

// ApplyReport.StateBytes counts no parked content: a park adds nothing, and
// the fill adds what lands, both exactly what the encoded state grew by.
func TestUnit_ParkedOutOfState_StateBytesCountsNoParkedContent(t *testing.T) {
	writer := New(WithClientID(7))
	txt := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ab", nil) })
	fill := EncodeStateAsUpdateV1(writer, nil)
	sv := writer.StateVector()
	writer.Transact(func(txn *Transaction) { txt.Insert(txn, 2, "cd", nil) })
	late := EncodeStateAsUpdateV1(writer, sv)

	room := New(WithClientID(99))
	room.SetParkedOutOfState(true)
	reported, grown := reportedGrowth(t, room, late)
	assert.Zero(t, grown, "a park leaves the state as it was")
	assert.Zero(t, reported, "and adds nothing to the count")
	reported, grown = reportedGrowth(t, room, fill)
	assert.Positive(t, grown)
	assert.Equal(t, grown, reported, "the fill and what its drain landed")
}

// Updates that arrive late, park and land, are counted exactly, as with the
// option off (TestUnit_ApplyReport_StateBytesIsExactForUpdatesThatArriveLate).
// Under the race detector it runs two seeds for each GC setting.
func TestUnit_ParkedOutOfState_StateBytesIsExactForUpdatesThatArriveLate(t *testing.T) {
	seeds := uint64(25)
	if raceEnabled {
		seeds = 2
	}
	parkedBefore := 0
	for _, gc := range []bool{false, true} {
		for seed := uint64(1); seed <= seeds; seed++ {
			rng := rand.New(rand.NewPCG(seed, 0xC0FFEE))
			room := New(WithClientID(1), WithGC(gc))
			room.SetParkedOutOfState(true)
			writers := make([]*Doc, 3)
			sent := make([]StateVector, len(writers))
			for i := range writers {
				writers[i] = New(WithClientID(ClientID(i+2)), WithGC(gc))
				sent[i] = writers[i].StateVector()
			}
			var held [][]byte
			deliver := func(u []byte) {
				if room.PendingStats().Items > 0 {
					parkedBefore++
				}
				if reported, grown := reportedGrowth(t, room, u); reported != grown {
					t.Fatalf("gc %v, seed %d: the report adds %d bytes to the encoded state, which grew by %d", gc, seed, reported, grown)
				}
			}
			for round := range 40 {
				for i, w := range writers {
					require.NoError(t, ApplyUpdateV1(w, EncodeStateAsUpdateV1(room, w.StateVector()), nil))
					w.Transact(func(txn *Transaction) { lateWriterEdit(rng, txn, i, round) })
					u := EncodeStateAsUpdateV1(w, sent[i])
					sent[i] = w.StateVector()
					if rng.IntN(4) == 0 {
						held = append(held, u)
						continue
					}
					deliver(u)
				}
				if len(held) > 0 && rng.IntN(2) == 0 {
					rng.Shuffle(len(held), func(a, b int) { held[a], held[b] = held[b], held[a] })
					for _, u := range held {
						deliver(u)
					}
					held = held[:0]
				}
			}
			for _, u := range held {
				deliver(u)
			}
		}
	}
	require.Positive(t, parkedBefore, "no update reached the room while it held parked structs")
}

// Re-sent states, in any mix of what the room holds and holds parked, are
// never counted short of what the state grew by. Under the race detector it
// runs a fifth of the seeds (raceSeeds), which still pair each GC setting of
// the writers with each of the room's.
func TestUnit_ParkedOutOfState_StateBytesNeverFallsShortOfRedeliveredState(t *testing.T) {
	clients := []ClientID{3, 200, 1 << 40}
	for seed := range uint64(raceSeeds(40)) {
		rng := rand.New(rand.NewPCG(seed, 7))
		writers := make([]*Doc, 0, len(clients))
		for _, c := range clients {
			writers = append(writers, New(WithClientID(c), WithGC(seed%2 == 0)))
		}
		states := make([][]byte, 0, 60)
		for step := range 60 {
			w := writers[rng.IntN(len(writers))]
			for _, o := range writers {
				if o != w {
					require.NoError(t, ApplyUpdateV1(w, EncodeStateAsUpdateV1(o, w.StateVector()), nil))
				}
			}
			w.Transact(func(txn *Transaction) { randomEdit(rng, txn, step) })
			states = append(states, EncodeStateAsUpdateV1(w, nil))
		}
		room := New(WithClientID(9), WithGC(seed%3 == 0))
		room.SetParkedOutOfState(true)
		for range 40 {
			sv := StateVector{}
			for _, c := range clients {
				sv[c] = uint64(rng.IntN(40))
			}
			u, err := DiffUpdateV1(states[rng.IntN(len(states))], sv)
			require.NoError(t, err)
			if reported, grown := reportedGrowth(t, room, u); reported < grown {
				t.Fatalf("seed %d: the report adds %d bytes to the encoded state, which grew by %d", seed, reported, grown)
			}
		}
	}
}
