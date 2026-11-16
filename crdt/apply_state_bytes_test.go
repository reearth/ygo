package crdt

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// Tests for ApplyReport.StateBytes: what each apply adds to the document's
// encoded state, which an owner sums to keep the state's length without
// encoding it.

// clockDeleteUpdate is an update of no structs whose delete set deletes
// length clocks of client from clock.
func clockDeleteUpdate(client ClientID, clock, length uint64) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0)              // no structs
	enc.WriteVarUint(1)              // one client in the delete set
	enc.WriteVarUint(uint64(client)) // the client whose clocks it deletes
	enc.WriteVarUint(1)              // one range
	enc.WriteVarUint(clock)
	enc.WriteVarUint(length)
	return bytes.Clone(enc.Bytes())
}

// stateLen is the length of doc's encoded state, the measure StateBytes keeps.
func stateLen(doc *Doc) int { return len(EncodeStateAsUpdateV1(doc, nil)) }

// applyReported applies update to doc as V1 or V2 under a report callback and
// returns the report the apply made and what the encoded state grew by. The
// apply must return wantErr, nil for one that succeeds.
func applyReported(t *testing.T, doc *Doc, update []byte, v2 bool, wantErr error) (ApplyReport, int) {
	t.Helper()
	var made reports
	doc.SetApplyReport(made.take)
	defer doc.SetApplyReport(nil)
	before := stateLen(doc)
	if err := applyEither(t, doc, update, v2); !errors.Is(err, wantErr) || (err == nil) != (wantErr == nil) {
		t.Fatalf("the update was applied with err %v, want %v", err, wantErr)
	}
	return made.only(t), stateLen(doc) - before
}

// statePeer is a writer whose every transaction is one update, as a browser
// sends each edit.
type statePeer struct{ doc *Doc }

func newStatePeer(t *testing.T, base string, client ClientID) *statePeer {
	t.Helper()
	p := &statePeer{doc: New(WithClientID(client))}
	if err := ApplyUpdateV1(p.doc, baseState(t, base), nil); err != nil {
		t.Fatalf("the peer refused the base state: %v", err)
	}
	return p
}

func (p *statePeer) edit(fn func(txn *Transaction)) []byte {
	sv := p.doc.StateVector()
	p.doc.Transact(fn)
	return EncodeStateAsUpdateV1(p.doc, sv)
}

func (p *statePeer) text(fn func(txn *Transaction, text *YText)) []byte {
	text := p.doc.GetText("t")
	return p.edit(func(txn *Transaction) { fn(txn, text) })
}

// gapGCUpdate is one GC struct of client at clock, length clocks long: what a
// peer that collected deleted content sends for it, here behind a gap when
// clock is past what the document holds of client.
func gapGCUpdate(client ClientID, clock, length uint64) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // one client
	enc.WriteVarUint(1) // one struct of it
	enc.WriteVarUint(uint64(client))
	enc.WriteVarUint(clock)
	enc.WriteUint8(0) // a GC struct
	enc.WriteVarUint(length)
	enc.WriteVarUint(0) // an empty delete set
	return bytes.Clone(enc.Bytes())
}

// stateShape is one kind of apply: the document it lands on, built from base by
// the updates before it, with whole-or-nothing applies where wholeOrNothing is
// set and bounded by budget where it is set, the update measured, and the
// error the apply returns, nil for one that succeeds.
type stateShape struct {
	name           string
	base           string
	before         func(t *testing.T) [][]byte
	update         func(t *testing.T) []byte
	budget         *ApplyBudget
	wholeOrNothing bool
	fails          error
}

// stateShapes are the applies the report must count to the byte: what a
// browser's edits send, and what parks behind a gap and lands later.
func stateShapes() []stateShape {
	const base = "hello world"
	run := strings.Repeat("abcdefghij", 1000) // 10,000 characters written as one struct
	none := func(*testing.T) [][]byte { return nil }
	return []stateShape{
		{
			name: "an ordinary insert", base: base, before: none,
			update: func(t *testing.T) []byte {
				return newStatePeer(t, base, 2).text(func(txn *Transaction, text *YText) { text.Insert(txn, 11, "!", nil) })
			},
		},
		{
			// The receiving document squashes no run across applies, so each
			// keystroke stays a struct of its own there.
			name: "the next keystroke of the same writer", base: base,
			before: func(t *testing.T) [][]byte {
				return [][]byte{keystrokes(t, base)[0]}
			},
			update: func(t *testing.T) []byte { return keystrokes(t, base)[1] },
		},
		{
			name: "a paste", base: base, before: none,
			update: func(t *testing.T) []byte {
				return newStatePeer(t, base, 2).text(func(txn *Transaction, text *YText) {
					text.Insert(txn, 11, strings.Repeat("pasted text ", 1000), nil)
				})
			},
		},
		{
			name: "a paste of many structs, which the commit squashes", base: base, before: none,
			update: func(t *testing.T) []byte {
				return newStatePeer(t, base, 2).text(func(txn *Transaction, text *YText) {
					for i := range 200 {
						text.Insert(txn, 11+i, "x", nil)
					}
				})
			},
		},
		{
			name: "an insert inside a run, which splits it", base: base, before: none,
			update: func(t *testing.T) []byte {
				return newStatePeer(t, base, 2).text(func(txn *Transaction, text *YText) { text.Insert(txn, 5, ",", nil) })
			},
		},
		{
			name: "a delete inside a run", base: base, before: none,
			update: func(t *testing.T) []byte {
				return newStatePeer(t, base, 2).text(func(txn *Transaction, text *YText) { text.Delete(txn, 4, 1) })
			},
		},
		{
			name: "a delete of every other character of a 10,000-character run", base: run, before: none,
			update: func(t *testing.T) []byte { return everyOther(t, run, 0, len(run)) },
		},
		{
			name: "a delete that joins the ranges of two earlier ones", base: base,
			before: func(t *testing.T) [][]byte {
				return [][]byte{clockDeleteUpdate(1, 3, 1), clockDeleteUpdate(1, 5, 1)}
			},
			update: func(*testing.T) []byte { return clockDeleteUpdate(1, 4, 1) },
		},
		{
			name: "a formatted range", base: base, before: none,
			update: func(t *testing.T) []byte {
				return newStatePeer(t, base, 2).text(func(txn *Transaction, text *YText) {
					text.Format(txn, 2, 5, Attributes{"bold": true})
				})
			},
		},
		{
			name: "an apply that only parks content behind a clock gap", base: base, before: none,
			update: func(*testing.T) []byte {
				return gapStringUpdate(5, strings.Repeat("x", 9000), &ID{Client: 2, Clock: 4})
			},
		},
		{
			name: "a second park into the gap of the first", base: base,
			before: func(*testing.T) [][]byte {
				return [][]byte{gapStringUpdate(5, strings.Repeat("x", 9000), &ID{Client: 2, Clock: 4})}
			},
			update: func(*testing.T) []byte { return gapStringUpdate(2, "yy", &ID{Client: 2, Clock: 1}) },
		},
		{
			name: "an apply that only parks GC structs", base: base, before: none,
			update: func(*testing.T) []byte { return gapGCUpdate(3, 200, 5) },
		},
		{
			name: "a GC struct the queue merges with one parked before it", base: base,
			before: func(*testing.T) [][]byte { return [][]byte{gapGCUpdate(3, 200, 5)} },
			update: func(*testing.T) []byte { return gapGCUpdate(3, 205, 300) },
		},
		{
			name: "an apply that only parks a delete", base: base, before: none,
			update: func(*testing.T) []byte { return clockDeleteUpdate(9, 2, 3) },
		},
		{
			name: "a drain that lands parked content", base: base,
			before: func(*testing.T) [][]byte { return [][]byte{parkedStructs(5)} },
			update: func(*testing.T) []byte { return gapStringUpdate(0, "A", nil) },
		},
		{
			name: "a drain whose landing splits a run", base: base,
			before: func(*testing.T) [][]byte { return [][]byte{parkedSplitters(10)} },
			update: func(*testing.T) []byte { return gapStringUpdate(0, "A", nil) },
		},
		{
			// The drain's splits pass the budget at the tenth, so the drain
			// stops: the fill and the nine characters it landed stand, and the
			// tenth waits in the queue.
			name: "a drain the budget stops, which keeps what it landed", base: base,
			before: func(*testing.T) [][]byte { return [][]byte{parkedSplitters(10)} },
			update: func(*testing.T) []byte { return gapStringUpdate(0, "A", nil) },
			budget: &ApplyBudget{Structs: 1, Bytes: 1, Split: 9},
		},
		{
			// With whole-or-nothing applies the stopped drain is taken back:
			// the fill stands, the run's cut is merged back by the commit,
			// and the ten characters wait in the queue as they did.
			name: "a drain the budget stops, taken back by whole-or-nothing applies", base: base,
			before:         func(*testing.T) [][]byte { return [][]byte{parkedSplitters(10)} },
			update:         func(*testing.T) []byte { return gapStringUpdate(0, "A", nil) },
			budget:         &ApplyBudget{Structs: 1, Bytes: 1, Split: 9},
			wholeOrNothing: true,
		},
		{
			// The budget stops the apply at the third insert, and the
			// document keeps the first two and the splits they made.
			name: "an apply the budget stops partway, which keeps what it integrated", base: base, before: none,
			update: func(t *testing.T) []byte { return threeInserts(t, base) },
			budget: &ApplyBudget{Structs: 2},
			fails:  ErrApplyBudget,
		},
		{
			name: "a subdocument", base: base, before: none,
			update: func(t *testing.T) []byte {
				p := newStatePeer(t, base, 2)
				m := p.doc.GetMap("m")
				return p.edit(func(txn *Transaction) {
					m.Set(txn, "sub", New(WithGUID("a subdocument"), WithGC(false), WithAutoLoad(true)))
				})
			},
		},
		{
			name: "a move of an array element", base: base,
			before: func(t *testing.T) [][]byte { return arrayMoves(t, base)[:1] },
			update: func(t *testing.T) []byte { return arrayMoves(t, base)[1] },
		},
		{
			name: "a drain that lands a GC struct", base: base,
			before: func(*testing.T) [][]byte { return [][]byte{gapGCUpdate(3, 10, 5)} },
			update: func(*testing.T) []byte { return gapGCUpdate(3, 0, 10) },
		},
		{
			name: "an apply that lands what a parked delete names", base: base,
			before: func(*testing.T) [][]byte { return [][]byte{clockDeleteUpdate(2, 0, 1)} },
			update: func(*testing.T) []byte { return gapStringUpdate(0, "AB", nil) },
		},
		{
			name: "map entries under keys of their own", base: base, before: none,
			update: func(t *testing.T) []byte {
				p := newStatePeer(t, base, 2)
				m := p.doc.GetMap("m")
				return p.edit(func(txn *Transaction) {
					m.Set(txn, "title", "a room")
					m.Set(txn, "count", 42)
					m.Set(txn, "tags", []any{"x", 1.5, map[string]any{"k": nil}})
				})
			},
		},
		{
			name: "a map entry set again", base: base,
			before: func(t *testing.T) [][]byte { return mapSets(t, base)[:1] },
			update: func(t *testing.T) []byte { return mapSets(t, base)[1] },
		},
		{
			name: "a delete of nested types, which the commit collects", base: "",
			before: func(t *testing.T) [][]byte { return [][]byte{mustHex(t, nestedFirst)} },
			update: func(t *testing.T) []byte { return mustHex(t, nestedFromA) },
		},
		{
			// The update re-delivers the struct of writer 3 the room holds
			// parked behind its gap, beside a struct of writer 4 that parks
			// too: the queue's merge drops the parked copy for the one that
			// arrives, whose skip in front of it the state keeps as it was.
			name: "a park whose merge replaces the parked struct it re-delivers", base: base,
			before: func(t *testing.T) [][]byte { return [][]byte{lateMapSet(t, 3, 0), lateMapSet(t, 3, 2)} },
			update: func(t *testing.T) []byte { return merged(t, lateMapSet(t, 4, 1), lateMapSet(t, 3, 2)) },
		},
		{
			// As above for a writer the store holds nothing of, whose
			// section the queue alone writes: its count, client and start
			// clock stay as they were.
			name: "a park whose merge replaces a parked struct of a writer only the queue holds", base: base,
			before: func(t *testing.T) [][]byte { return [][]byte{lateMapSet(t, 1<<40, 200)} },
			update: func(t *testing.T) []byte { return merged(t, lateMapSet(t, 1<<41, 1), lateMapSet(t, 1<<40, 200)) },
		},
		{
			// The parked GC struct at clock 8 takes in the one that arrives
			// for clocks 9 to 12, which closes the skip in front of the item
			// parked at clock 12.
			name: "a GC struct the queue's merge grows up to the next struct it holds", base: base,
			before: func(t *testing.T) [][]byte { return [][]byte{gapGCUpdate(5, 8, 1), lateMapSet(t, 5, 12)} },
			update: func(*testing.T) []byte { return gapGCUpdate(5, 9, 3) },
		},
		{
			name: "a GC struct the queue's merge grows partway into the skip after it", base: base,
			before: func(t *testing.T) [][]byte { return [][]byte{gapGCUpdate(5, 8, 1), lateMapSet(t, 5, 399)} },
			update: func(*testing.T) []byte { return gapGCUpdate(5, 9, 300) },
		},
	}
}

// lateMapSet is the struct a writer of client makes at clock, alone: its
// clock+1st set of a key of its own in map "m", as it sends it to a document
// that holds its clocks before it, and one that does not parks it.
func lateMapSet(t *testing.T, client ClientID, clock uint64) []byte {
	t.Helper()
	w := New(WithClientID(client))
	m := w.GetMap("m")
	for i := range clock + 1 {
		w.Transact(func(txn *Transaction) { m.Set(txn, fmt.Sprint("k", i), int64(i)) })
	}
	return EncodeStateAsUpdateV1(w, StateVector{client: clock})
}

// merged is updates as one V1 update.
func merged(t *testing.T, updates ...[]byte) []byte {
	t.Helper()
	u, err := MergeUpdatesV1(updates...)
	if err != nil {
		t.Fatalf("merge the updates: %v", err)
	}
	return u
}

// keystrokes is two one-character inserts at the end of base, each its own
// update from the same writer.
func keystrokes(t *testing.T, base string) [][]byte {
	t.Helper()
	p := newStatePeer(t, base, 2)
	first := p.text(func(txn *Transaction, text *YText) { text.Insert(txn, len(base), "a", nil) })
	second := p.text(func(txn *Transaction, text *YText) { text.Insert(txn, len(base)+1, "b", nil) })
	return [][]byte{first, second}
}

// mapSets is key "k" of map "m" set twice by one writer, each its own update.
func mapSets(t *testing.T, base string) [][]byte {
	t.Helper()
	p := newStatePeer(t, base, 2)
	m := p.doc.GetMap("m")
	first := p.edit(func(txn *Transaction) { m.Set(txn, "k", strings.Repeat("v", 300)) })
	second := p.edit(func(txn *Transaction) { m.Set(txn, "k", "w") })
	return [][]byte{first, second}
}

// arrayMoves is an array of three values a writer pushes, then its first
// value moved to the end, each its own update.
func arrayMoves(t *testing.T, base string) [][]byte {
	t.Helper()
	p := newStatePeer(t, base, 2)
	arr := p.doc.GetArray("a")
	first := p.edit(func(txn *Transaction) { arr.Push(txn, []any{"x", "y", "z"}) })
	second := p.edit(func(txn *Transaction) { arr.Move(txn, 0, 3) })
	return [][]byte{first, second}
}

// everyOther is one update deleting every other character of text in
// [from, to), as one transaction of the writer.
func everyOther(t *testing.T, text string, from, to int) []byte {
	t.Helper()
	return newStatePeer(t, text, 2).text(func(txn *Transaction, yt *YText) {
		for i := to - 1 - (to-1-from)%2; i >= from; i -= 2 {
			yt.Delete(txn, i, 1)
		}
	})
}

// shapeDoc is a document holding shape's base with the updates before the
// measured one applied, and no report callback yet.
func shapeDoc(t *testing.T, shape stateShape) *Doc {
	t.Helper()
	doc := New(WithClientID(7), WithWholeOrNothingApplies(shape.wholeOrNothing))
	if shape.base != "" {
		doc = contentDoc(t, shape.base, shape.wholeOrNothing)
	}
	for _, u := range shape.before(t) {
		if err := ApplyUpdateV1(doc, u, nil); err != nil {
			t.Fatalf("an update before the measured one was refused: %v", err)
		}
	}
	if shape.budget != nil {
		budget := *shape.budget
		doc.SetApplyBudget(func(any) ApplyBudget { return budget })
	}
	return doc
}

// Each apply an owner's room takes reports, as StateBytes, exactly what it
// added to the room's encoded state: a struct it integrated or parked, a split
// it made, a range it added to the delete set, less what the commit's merges
// and collection took out - decoded as V1 and as V2 alike, since the state it
// measures is V1's either way.
func TestUnit_ApplyReport_StateBytesIsWhatTheApplyAddedToTheEncodedState(t *testing.T) {
	for _, shape := range stateShapes() {
		for name, v2 := range map[string]bool{"V1": false, "V2": true} {
			t.Run(shape.name+"/"+name, func(t *testing.T) {
				doc := shapeDoc(t, shape)
				report, grown := applyReported(t, doc, shape.update(t), v2, shape.fails)
				t.Logf("StateBytes %d, the encoded state grew by %d, Bytes %d", report.StateBytes, grown, report.Bytes)
				if report.StateBytes != grown {
					t.Fatalf("the report adds %d bytes to the encoded state, which grew by %d", report.StateBytes, grown)
				}
			})
		}
	}
}

// The delete set's counts the report bounds rather than keeps put it above the
// state by the bound's byte, never below. A delete
// that adds a range to a client of more than 254 structs may cross the varuint
// counting the client's ranges, which the document does not keep, so the
// report takes the byte the crossing would cost: one byte over the state for
// the 1,001st range of a client, which crosses nothing, and exact for the
// 128th, which crosses from one byte to two.
func TestUnit_ApplyReport_StateBytesTakesTheRangeCountsBoundAboveTheState(t *testing.T) {
	run := strings.Repeat("abcdefghij", 300)
	for _, c := range []struct {
		name     string
		deleted  int // every other character of the first deleted is deleted before
		over     int
		rangesAt int
	}{
		{"the 1,001st range", 2000, 1, 1000},
		{"the 128th range", 254, 0, 127},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := contentDoc(t, run, false)
			if err := ApplyUpdateV1(doc, everyOther(t, run, 0, c.deleted), nil); err != nil {
				t.Fatalf("the deletes before the measured one were refused: %v", err)
			}
			if got := len(buildDeleteSet(doc.store).clients[1]); got != c.rangesAt {
				t.Fatalf("the writer holds %d delete ranges before the measured delete, want %d", got, c.rangesAt)
			}
			more := newStatePeer(t, run, 3).text(func(txn *Transaction, text *YText) { text.Delete(txn, 2500, 1) })
			report, grown := applyReported(t, doc, more, false, nil)
			t.Logf("StateBytes %d, the encoded state grew by %d", report.StateBytes, grown)
			if report.StateBytes != grown+c.over {
				t.Fatalf("the report adds %d bytes, want the state's growth of %d and %d over it", report.StateBytes, grown, c.over)
			}
		})
	}
}

// A room's encoded state is the sum of what its applies reported, however the
// updates arrive: four writers - their client IDs one to six bytes long, as a
// browser's random ones are - edit text, a map and an array at once, and each
// edit reaches the room as its own update, some held back and delivered after
// later ones, so the room parks them behind their gaps, parks deletes of
// clocks it has not seen, and drains both when the gap is filled; on a room
// that collects deleted content and on one that keeps it.
func TestUnit_ApplyReport_StateBytesSumsToTheEncodedStateUnderReordering(t *testing.T) {
	const base = "seed text for the room"
	for seed := range uint64(8) {
		rng := rand.New(rand.NewPCG(seed, 99))
		room := New(WithClientID(7), WithGC(seed%2 == 0))
		if err := ApplyUpdateV1(room, baseState(t, base), nil); err != nil {
			t.Fatalf("the base state was refused: %v", err)
		}
		peers := make([]*statePeer, 0, 4)
		for _, c := range []ClientID{10, 200, 3_000_000_000, 1 << 40} {
			peers = append(peers, newStatePeer(t, base, c))
		}
		var held [][]byte
		deliver := func(u []byte) {
			report, grown := applyReported(t, room, u, rng.IntN(2) == 0, nil)
			if report.StateBytes != grown {
				t.Fatalf("seed %d: the report adds %d bytes to the encoded state, which grew by %d", seed, report.StateBytes, grown)
			}
		}
		for step := range 250 {
			p := peers[rng.IntN(len(peers))]
			u := p.edit(func(txn *Transaction) { randomEdit(rng, txn, step) })
			// Peers see each other's edits, so they write beside them.
			for _, q := range peers {
				if q != p {
					if err := ApplyUpdateV1(q.doc, u, nil); err != nil {
						t.Fatalf("seed %d: a peer refused an update: %v", seed, err)
					}
				}
			}
			if rng.IntN(3) == 0 {
				held = append(held, u)
				continue
			}
			deliver(u)
			if len(held) > 0 && rng.IntN(3) == 0 {
				i := rng.IntN(len(held))
				deliver(held[i])
				held = append(held[:i], held[i+1:]...)
			}
		}
		for _, u := range held {
			deliver(u)
		}
	}
}

// randomEdit is one browser edit: text typed, pasted, embedded or deleted, a
// map key set, an array grown by a value or a nested map, or cut.
func randomEdit(rng *rand.Rand, txn *Transaction, step int) {
	text, m, arr := txn.GetText("t"), txn.GetMap("m"), txn.GetArray("a")
	switch rng.IntN(12) {
	case 0, 1:
		text.Insert(txn, rng.IntN(text.Len()+1), "k", nil)
	case 2:
		text.Insert(txn, rng.IntN(text.Len()+1), strings.Repeat("paste ", 1+rng.IntN(40)), nil)
	case 3:
		if n := text.Len(); n > 0 {
			i := rng.IntN(n)
			text.Delete(txn, i, 1+rng.IntN(min(n-i, 30)))
		}
	case 4:
		m.Set(txn, []string{"a", "b", "c", "d"}[rng.IntN(4)], []any{step, "v", float64(step) / 3}[rng.IntN(3)])
	case 5:
		arr.Insert(txn, rng.IntN(arr.Len()+1), []any{step, map[string]any{"s": strings.Repeat("e", rng.IntN(200))}})
	case 6:
		text.InsertEmbed(txn, rng.IntN(text.Len()+1), map[string]any{"img": strings.Repeat("u", rng.IntN(50))}, nil)
	case 7:
		nested := NewMapPrelim()
		arr.InsertType(txn, rng.IntN(arr.Len()+1), nested)
		nested.Set(txn, "x", step)
		nested.Set(txn, "y", strings.Repeat("n", rng.IntN(30)))
	case 8:
		if n := text.Len(); n > 0 {
			text.Delete(txn, 0, 1+rng.IntN(n))
		}
	case 9:
		if n := arr.Len(); n > 0 {
			arr.Delete(txn, rng.IntN(n), 1)
		}
	case 10:
		if n := arr.Len(); n > 1 {
			arr.Move(txn, rng.IntN(n), rng.IntN(n+1))
		}
	default:
		m.Set(txn, "sub", New(WithGUID(fmt.Sprint("sub", step)), WithGC(rng.IntN(2) == 0)))
	}
}

// subdocWithMeta is a subdocument holding every option writeSubdocOpts
// writes: gc off, autoLoad on, and a meta value with an integer past int32.
func subdocWithMeta() *Doc {
	d := New(WithGUID("a subdocument with options"), WithGC(false), WithAutoLoad(true))
	d.meta = map[string]any{"title": "notes", "n": int64(1 << 40)}
	return d
}

// StateBytes counts the bytes the encode writes, which this test writes out
// for a document whose state it knows, so the count is checked against
// numbers and not only against an encode.
//
// "hello world" written by client 1 in one struct is 21 bytes: the client
// count (1), client 1's section header - its struct count, the client and
// the start clock (3) - the struct's info byte, parent info and root name "t"
// (1+1+2) and its text (1+11), and an empty delete set's client count (1).
//
// Client 2 appends "!": a section of its own (3) holding a struct of an info
// byte, its left origin, clock 10 of client 1 (2), and "!" (2), 8 bytes. Client
// 3 deletes "o": the run is cut in three, "hell" under its parent (9), the
// deleted "o" after its left origin and collected to its length (4) and
// " world" after its own (10), 7 more than the 16 the run took, and the delete
// set gains client 1 with one range of clock 4 and length 1 (4), 11 bytes.
// A struct of client 2 at clock 5 after clock 4 parks behind the gap of clocks
// 1 to 4: the skip struct that stands for the gap (2) and the struct, an
// info byte, its origin (2) and "yz" (3), 8 bytes.
func TestUnit_ApplyReport_StateBytesCountsTheBytesTheEncodeWrites(t *testing.T) {
	const base = "hello world"
	doc := contentDoc(t, base, false)
	if got := stateLen(doc); got != 21 {
		t.Fatalf("the base state is %d bytes, want 21", got)
	}
	steps := []struct {
		name   string
		update []byte
		want   int
	}{
		{"client 2 appends \"!\"", newStatePeer(t, base, 2).text(func(txn *Transaction, text *YText) { text.Insert(txn, 11, "!", nil) }), 8},
		{"client 3 deletes \"o\"", newStatePeer(t, base, 3).text(func(txn *Transaction, text *YText) { text.Delete(txn, 4, 1) }), 11},
		{"client 2 parks \"yz\"", gapStringUpdate(5, "yz", &ID{Client: 2, Clock: 4}), 8},
	}
	total := 21
	for _, s := range steps {
		report, grown := applyReported(t, doc, s.update, false, nil)
		total += report.StateBytes
		if report.StateBytes != s.want || grown != s.want {
			t.Fatalf("%s: the report adds %d bytes and the state grew by %d, want %d", s.name, report.StateBytes, grown, s.want)
		}
	}
	if got := stateLen(doc); got != total || total != 21+8+11+8 {
		t.Fatalf("the state is %d bytes and the reports sum to %d, want %d", got, total, 21+8+11+8)
	}
}

// A parked struct the drain drops, under a root the list of the apply that
// parked it leaves out, leaves the state with the apply that filled its gap,
// which stands; that apply's report takes it off, all but a skip that may
// stand for its clocks and its client's section header, which the state may
// drop with it. A sender allowed only "m" parks a write into "t" behind an
// item it has and the document lacks, and a trusted peer's update brings that
// item.
//
// The park is exact: client 4's section header (3) and the struct, an info
// byte, its origin (2) and "Q" (2), 8 bytes. The fill adds client 3's section
// (3) and "Z" under root "t" (6), and the drop takes client 4's section away
// whole: the state grows by 1. The report takes off the struct less the skip
// of its one clock (5 - 2) and leaves the section header (3) counted, 5 bytes
// above the state and never below it.
func TestUnit_ApplyReport_StateBytesBoundsAParkedStructTheDrainDrops(t *testing.T) {
	trusted := New(WithClientID(3))
	trustedText := trusted.GetText("t")
	trusted.Transact(func(txn *Transaction) { trustedText.Insert(txn, 0, "Z", nil) })
	privateItem := EncodeStateAsUpdateV1(trusted, nil)

	sender := New(WithClientID(4))
	if err := ApplyUpdateV1(sender, privateItem, nil); err != nil {
		t.Fatal(err)
	}
	senderText := sender.GetText("t")
	sender.Transact(func(txn *Transaction) { senderText.Insert(txn, 1, "Q", nil) })
	forged := EncodeStateAsUpdateV1(sender, trusted.StateVector())

	doc := New(WithClientID(7), WithAllowedRoots(senderOnlyM))
	for _, step := range []struct {
		name          string
		update        []byte
		origin        string
		grown, report int
	}{
		{"the sender's write parks", forged, "sender", 8, 8},
		{"the trusted update lands and the drain drops the write", privateItem, "trusted", 1, 6},
	} {
		var made reports
		doc.SetApplyReport(made.take)
		before := stateLen(doc)
		if err := ApplyUpdateV1(doc, step.update, step.origin); err != nil {
			t.Fatalf("%s: the update was refused: %v", step.name, err)
		}
		report, grown := made.only(t), stateLen(doc)-before
		if grown != step.grown || report.StateBytes != step.report {
			t.Fatalf("%s: the report adds %d bytes and the state grew by %d, want %d and %d",
				step.name, report.StateBytes, grown, step.report, step.grown)
		}
	}
	if got := doc.PendingStats().RootsDropped; len(got) != 1 || got[4] != 1 {
		t.Fatalf("the drops are reported as %v, want one for client 4", got)
	}
}

// structLen is the length encodeItem writes, for every content a struct can
// carry and every header shape: the report's count is only as exact as this.
func TestUnit_StructLenIsWhatEncodeItemWrites(t *testing.T) {
	doc := New(WithClientID(1))
	root := doc.GetText("t").baseType()
	nested := &Item{ID: ID{Client: 4, Clock: 9}}
	child := &abstractType{item: nested}
	key := "a key"
	element := NewYXmlElement("paragraph")
	contents := []Content{
		NewContentString("plain text"),
		NewContentString("emoji 😀 and ünïcode"),
		NewContentDeleted(300),
		&ContentAny{Vals: []any{nil, true, false, 0, -1, 63, 64, -8191, int64(1 << 40), int64(-(1 << 60)), uint64(1 << 63), int8(3), uint16(70000 % 65536),
			float32(1.5), 1.5, 0.1, encoding.BigInt(7), "s", []byte{1, 2, 3}, []any{1, "x", []any{}}, map[string]any{"b": 1, "a": "z"}}},
		&ContentJSON{Vals: []any{map[string]any{"x": 1.0}, "quoted \"text\"", nil}},
		&ContentBinary{Data: make([]byte, 200)},
		&ContentEmbed{Val: map[string]any{"image": "https://example.com/x.png"}},
		&ContentFormat{Key: "bold", Val: true},
		&ContentType{Type: NewMapPrelim().baseType()},
		&ContentType{Type: element.baseType()},
		&ContentMove{Target: &ID{Client: 300, Clock: 70000}, TargetLen: 2},
		&ContentDoc{Doc: New(WithGUID("a subdocument"))},
		&ContentDoc{Doc: subdocWithMeta()},
	}
	headers := []func(it *Item){
		func(it *Item) { it.Parent = root },
		func(it *Item) { it.Parent, it.ParentSub = root, &key },
		func(it *Item) { it.Parent = child },
		func(it *Item) { it.parentID = &ID{Client: 1 << 33, Clock: 5} },
		func(it *Item) { it.Parent, it.Origin = root, &ID{Client: 1 << 32, Clock: 1 << 20} },
		func(it *Item) { it.Parent, it.OriginRight = root, &ID{Client: 2, Clock: 0} },
		func(it *Item) {
			it.Parent, it.Origin, it.OriginRight, it.ParentSub = root, &ID{Client: 2, Clock: 3}, &ID{Client: 5, Clock: 200}, &key
		},
		func(it *Item) { it.Deleted, it.Content = true, NewContentDeleted(it.Content.Len()) }, // a GC struct
	}
	for ci, content := range contents {
		for hi, header := range headers {
			for _, offset := range []uint64{0, 1, 3, 7} { // 7 cuts the emoji's surrogate pair
				it := &Item{ID: ID{Client: 1 << 31, Clock: 1 << 14}, Content: content.Copy()}
				if cs, ok := content.(*ContentString); ok {
					it.Content = NewContentString(cs.Str)
				}
				header(it)
				if offset >= it.Content.Len() {
					continue
				}
				enc := encoding.NewEncoder()
				encodeItem(enc, it, offset)
				if got, want := structLen(it, offset, true), len(enc.Bytes()); got != want {
					t.Errorf("content %d, header %d, offset %d: structLen %d, encodeItem wrote %d", ci, hi, offset, got, want)
				}
			}
		}
	}
}

// reportedGrowth applies update to doc as V1 under a report callback and
// returns what the apply reported - nothing for one that committed nothing -
// and what the encoded state grew by.
func reportedGrowth(t *testing.T, doc *Doc, update []byte) (reported, grown int) {
	t.Helper()
	return reportedGrowthBy(t, doc, update, ApplyUpdateV1)
}

// reportedGrowthBy is reportedGrowth with apply, V1's or V2's.
func reportedGrowthBy(t *testing.T, doc *Doc, update []byte, apply func(*Doc, []byte, any) error) (reported, grown int) {
	t.Helper()
	var made reports
	doc.SetApplyReport(made.take)
	defer doc.SetApplyReport(nil)
	before := stateLen(doc)
	if err := apply(doc, update, nil); err != nil {
		t.Fatalf("the update was refused: %v", err)
	}
	switch len(made.made) {
	case 0:
	case 1:
		reported = made.made[0].StateBytes
	default:
		t.Fatalf("%d reports were made, want at most one: %+v", len(made.made), made.made)
	}
	return reported, stateLen(doc) - before
}

// lateWriterEdit is one edit of a writer whose updates reach the room late:
// text typed, a short delete, a character bolded or unbolded, a value pushed
// to an array, a map key set, or four deletes a character apart, which split
// the run they fall in.
func lateWriterEdit(rng *rand.Rand, txn *Transaction, writer, round int) {
	text, arr, m := txn.GetText("t"), txn.GetArray("a"), txn.GetMap("m")
	n := text.Len()
	switch rng.IntN(7) {
	case 0, 1:
		text.Insert(txn, rng.IntN(n+1), fmt.Sprintf("w%d.%d-", writer, round), nil)
	case 2:
		if n > 2 {
			at := rng.IntN(n - 1)
			text.Delete(txn, at, min(1+rng.IntN(min(3, n-1)), n-at))
		}
	case 3:
		if n > 2 {
			text.Format(txn, rng.IntN(n-1), 1, Attributes{"bold": rng.IntN(2) == 0})
		}
	case 4:
		arr.Push(txn, []any{fmt.Sprintf("v%d.%d", writer, round)})
	case 5:
		m.Set(txn, fmt.Sprint("k", rng.IntN(4)), round)
	default:
		if n > 10 {
			from := rng.IntN(n - 8)
			for k := range 4 {
				text.Delete(txn, from+k, 1)
			}
		}
	}
}

// The report is exact, apply by apply, for writers one update in four of
// whose reaches the room late: three writers edit, each first taking in all
// the room holds - what it holds parked too - so each update re-sends
// structs of other writers that the room holds parked, and the room's merge
// of its queue drops the parked copy for the one that arrives. Before the
// report read the queue's clocks ahead of that merge, 10 of these 6,000
// applies reported 2 bytes over the state: the skip in front of the dropped
// struct, which the state keeps, counted again for the struct that took its
// place. No client here holds 255 structs and no parked struct shares clocks
// with the store, so no bound of the report's applies and the count is the
// state's to the byte.
func TestUnit_ApplyReport_StateBytesIsExactForUpdatesThatArriveLate(t *testing.T) {
	parkedBefore := 0
	for _, gc := range []bool{false, true} {
		for seed := uint64(1); seed <= 25; seed++ {
			rng := rand.New(rand.NewPCG(seed, 0xC0FFEE))
			room := New(WithClientID(1), WithGC(gc))
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
					if err := ApplyUpdateV1(w, EncodeStateAsUpdateV1(room, w.StateVector()), nil); err != nil {
						t.Fatalf("gc %v, seed %d: a writer refused the room's state: %v", gc, seed, err)
					}
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
	if parkedBefore == 0 {
		t.Fatal("no update reached the room while it held parked structs, so the queue's merge went unmeasured")
	}
}

// The report never falls short of the state where updates re-send, in any
// mix, what the room holds and holds parked: three writers, client IDs of one
// to six bytes, edit and keep their whole state after each edit, and the room
// takes each state less a random state vector of the writers' clocks. Before
// the report read the queue's clocks ahead of its merge, 129 of 12,000 such
// applies reported less than the state grew by. What it reports above the
// state here is the bound of a struct appended at clocks a parked struct
// holds (overlapSlack).
func TestUnit_ApplyReport_StateBytesNeverFallsShortOfRedeliveredState(t *testing.T) {
	clients := []ClientID{3, 200, 1 << 40}
	for seed := range uint64(40) {
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
					if err := ApplyUpdateV1(w, EncodeStateAsUpdateV1(o, w.StateVector()), nil); err != nil {
						t.Fatalf("seed %d: a writer refused another's state: %v", seed, err)
					}
				}
			}
			w.Transact(func(txn *Transaction) { randomEdit(rng, txn, step) })
			states = append(states, EncodeStateAsUpdateV1(w, nil))
		}
		room := New(WithClientID(9), WithGC(seed%3 == 0))
		for range 40 {
			sv := StateVector{}
			for _, c := range clients {
				sv[c] = uint64(rng.IntN(40))
			}
			u, err := DiffUpdateV1(states[rng.IntN(len(states))], sv)
			if err != nil {
				t.Fatalf("seed %d: diff a writer's state: %v", seed, err)
			}
			if reported, grown := reportedGrowth(t, room, u); reported < grown {
				t.Fatalf("seed %d: the report adds %d bytes to the encoded state, which grew by %d", seed, reported, grown)
			}
		}
	}
}

// wideInts is a V1 update of one struct of client at clock: key of root map
// "m" set to n integers of 2^40, each written as lib0 type 125 and a varint.
// lib0 writes an integer past int32 as a float, but a peer may write one this
// way, and the document then holds an int64, which V1 writes as a float64 in
// nine bytes, while the merge an encode with parked state makes reads it back
// as the float64 and writes it in five, as a float32 holds it exactly.
func wideInts(client ClientID, clock uint64, key string, n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // one client
	enc.WriteVarUint(1) // one struct of it
	enc.WriteVarUint(uint64(client))
	enc.WriteVarUint(clock)
	enc.WriteUint8(wireAny | flagHasParentSub) // no origins: a named parent and a key
	enc.WriteUint8(1)
	enc.WriteVarString("m")
	enc.WriteVarString(key)
	enc.WriteVarUint(uint64(n))
	for range n {
		enc.WriteUint8(125)
		enc.WriteVarInt(1 << 40)
	}
	enc.WriteVarUint(0) // an empty delete set
	return bytes.Clone(enc.Bytes())
}

// wideIntsHeld is how many int64s past int32 the room's encoded state writes:
// the store's and the queue's ContentAny values.
func wideIntsHeld(doc *Doc) int {
	n := 0
	count := func(it *Item) {
		if ca, ok := it.Content.(*ContentAny); ok {
			for _, v := range ca.Vals {
				if i, ok := v.(int64); ok && i != int64(int32(i)) {
					n++
				}
			}
		}
	}
	for _, items := range doc.store.clients {
		for _, it := range items {
			count(it)
		}
	}
	if doc.store.pending != nil {
		for _, it := range doc.store.pending.heldItems() {
			count(it)
		}
	}
	return n
}

// embedsHeld is how many embeds the room's encoded state writes, each here
// one integer a float64 rounds up a digit.
func embedsHeld(doc *Doc) int {
	n := 0
	for _, items := range doc.store.clients {
		for _, it := range items {
			if _, ok := it.Content.(*ContentEmbed); ok {
				n++
			}
		}
	}
	return n
}

// A total an owner keeps from the reports is at or above the state in both
// forms EncodeStateAsUpdateV1 writes, after every apply: the direct encode
// while nothing is pending, and its merge with the parked parts otherwise
// (withParked). Two classes of value are written at different lengths in the
// two, and the report counts each at its longer one from the apply that
// brings it: an integer past int32 that a float32 holds exactly is nine bytes
// in the direct form and five merged, and an integer in an embed that a
// float64 rounds up a digit, 9999999999999999 from a V2 update, is a byte
// longer merged. So the total runs four bytes a wide integer above a merged
// state and a byte an embed above a direct one, across new writers' sets,
// which overwrite a key's values and collect them, sets and deletes parked
// behind their writers' gaps, the fills that land them, embeds and their
// deletes.
func TestUnit_ApplyReport_StateBytesTotalStaysAtOrAboveTheStateInBothForms(t *testing.T) {
	keys := []string{"a", "b", "c"}
	most, mostF, mostG := 0, 0, 0
	for seed := range uint64(30) {
		rng := rand.New(rand.NewPCG(seed, 5))
		room := New(WithClientID(1), WithGC(seed%2 == 0))
		deleter := New(WithClientID(2))
		total := stateLen(room)
		next := ClientID(100)
		var parkedSets, parkedDeletes []ClientID
		for step := range 80 {
			var u []byte
			apply := ApplyUpdateV1
			switch c := rng.IntN(7); {
			case c < 2:
				u = wideInts(next, 0, keys[rng.IntN(len(keys))], 1+rng.IntN(6))
			case c == 2:
				u = wideInts(next, 1, keys[rng.IntN(len(keys))], 1+rng.IntN(6))
				parkedSets = append(parkedSets, next)
			case c == 3:
				u = clockDeleteUpdate(next, 0, 1)
				parkedDeletes = append(parkedDeletes, next)
			case c == 4:
				peer := New(WithClientID(next))
				text := peer.GetText("t")
				peer.Transact(func(txn *Transaction) {
					text.InsertEmbed(txn, 0, map[string]any{"v": int64(9999999999999999)}, nil)
				})
				u, apply = EncodeStateAsUpdateV2(peer, nil), ApplyUpdateV2
			case c == 5:
				if err := ApplyUpdateV1(deleter, EncodeStateAsUpdateV1(room, nil), nil); err != nil {
					t.Fatalf("seed %d: the deleter refused the room's state: %v", seed, err)
				}
				sv, text := deleter.StateVector(), deleter.GetText("t")
				deleter.Transact(func(txn *Transaction) {
					if text.Len() > 0 {
						text.Delete(txn, 0, 1)
					}
				})
				u = EncodeStateAsUpdateV1(deleter, sv)
			case len(parkedSets) > 0 && (len(parkedDeletes) == 0 || rng.IntN(2) == 0):
				i := rng.IntN(len(parkedSets))
				u = wideInts(parkedSets[i], 0, keys[rng.IntN(len(keys))], 1)
				parkedSets = append(parkedSets[:i], parkedSets[i+1:]...)
			case len(parkedDeletes) > 0:
				i := rng.IntN(len(parkedDeletes))
				u = wideInts(parkedDeletes[i], 0, keys[rng.IntN(len(keys))], 1+rng.IntN(6))
				parkedDeletes = append(parkedDeletes[:i], parkedDeletes[i+1:]...)
			default:
				u = wideInts(next, 0, keys[rng.IntN(len(keys))], 1)
			}
			next++
			reported, _ := reportedGrowthBy(t, room, u, apply)
			total += reported
			state := EncodeStateAsUpdateV1(room, nil)
			merged, err := MergeUpdatesV1(state)
			if err != nil {
				t.Fatalf("seed %d, step %d: merge the state: %v", seed, step, err)
			}
			if total < len(state) || total < len(merged) {
				t.Fatalf("seed %d, step %d: the total is %d, below the state's encode of %d or its merged form of %d", seed, step, total, len(state), len(merged))
			}
			f, g := wideIntsHeld(room), embedsHeld(room)
			want, form := 4*f, "merged"
			if p := room.PendingStats(); p.Items == 0 && p.DeleteRanges == 0 {
				want, form = g, "direct"
			}
			if margin := total - len(state); margin != want {
				t.Fatalf("seed %d, step %d: the total runs %d bytes above the %s state, want %d for %d wide integers and %d embeds", seed, step, margin, form, want, f, g)
			} else if margin > most {
				most, mostF, mostG = margin, f, g
			}
		}
	}
	t.Logf("the largest margin: the total %d bytes above the state, holding %d wide integers and %d embeds", most, mostF, mostG)
}
