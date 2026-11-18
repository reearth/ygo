package crdt

import (
	"encoding/base64"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// A merge of non-contiguous updates from one client carries a skip struct
// over the missing clocks. Applying it alongside the update that fills the
// gap must converge on the full document, in either arrival order — the
// websocket persistence worker produces exactly these gapped batches when a
// stranded write takes an update out of the middle of its queue (#251).
// Expected outcomes are yjs 13.6.30's for the identical bytes.

type skipGapFormat struct {
	name  string
	conv  func([]byte) ([]byte, error)
	merge func(...[]byte) ([]byte, error)
	apply func(*Doc, []byte, any) error
}

var skipGapFormats = []skipGapFormat{
	{"V1", func(u []byte) ([]byte, error) { return u, nil }, MergeUpdatesV1, ApplyUpdateV1},
	{"V2", UpdateV1ToV2, MergeUpdatesV2, ApplyUpdateV2},
}

// threeUpdates records the V1 update of each of three sequential transactions.
func threeUpdates(t *testing.T, edit func(doc *Doc, i int)) [][]byte {
	t.Helper()
	src := New()
	var ups [][]byte
	unsub := src.OnUpdate(func(u []byte, _ any) { ups = append(ups, append([]byte(nil), u...)) })
	for i := 0; i < 3; i++ {
		edit(src, i)
	}
	unsub()
	require.Len(t, ups, 3)
	return ups
}

// applyGapped applies merge(u0, u2) and u1 to fresh docs in both orders.
func applyGapped(t *testing.T, f skipGapFormat, v1 [][]byte, read func(*Doc) string) (gappedFirst, fillerFirst string) {
	t.Helper()
	ups := make([][]byte, len(v1))
	for i, u := range v1 {
		c, err := f.conv(u)
		require.NoError(t, err)
		ups[i] = c
	}
	gapped, err := f.merge(ups[0], ups[2])
	require.NoError(t, err)

	run := func(order ...[]byte) string {
		d := New()
		for _, u := range order {
			require.NoError(t, f.apply(d, u, nil))
		}
		return read(d)
	}
	return run(gapped, ups[1]), run(ups[1], gapped)
}

func TestUnit_ApplyUpdate_GappedMergeThenFiller_MapKeys(t *testing.T) {
	keys := []string{"a", "b", "c"}
	v1 := threeUpdates(t, func(doc *Doc, i int) {
		doc.Transact(func(txn *Transaction) { txn.GetMap("m").Set(txn, keys[i], "1") })
	})
	read := func(d *Doc) string {
		ks := d.GetMap("m").Keys()
		sort.Strings(ks)
		return fmtKeys(ks)
	}
	for _, f := range skipGapFormats {
		t.Run(f.name, func(t *testing.T) {
			gappedFirst, fillerFirst := applyGapped(t, f, v1, read)
			require.Equal(t, "a,b,c", gappedFirst, "gapped merge, then filler")
			require.Equal(t, "a,b,c", fillerFirst, "filler, then gapped merge")
		})
	}
}

func TestUnit_ApplyUpdate_GappedMergeThenFiller_TextAppends(t *testing.T) {
	parts := []string{"ab", "cd", "ef"}
	v1 := threeUpdates(t, func(doc *Doc, i int) {
		doc.Transact(func(txn *Transaction) {
			txt := txn.GetText("t")
			txt.Insert(txn, txt.Len(), parts[i], nil)
		})
	})
	read := func(d *Doc) string { return d.GetText("t").ToString() }
	for _, f := range skipGapFormats {
		t.Run(f.name, func(t *testing.T) {
			gappedFirst, fillerFirst := applyGapped(t, f, v1, read)
			require.Equal(t, "abcdef", gappedFirst, "gapped merge, then filler")
			require.Equal(t, "abcdef", fillerFirst, "filler, then gapped merge")
		})
	}
}

func fmtKeys(ks []string) string {
	out := ""
	for i, k := range ks {
		if i > 0 {
			out += ","
		}
		out += k
	}
	return out
}

// Reported on PR #257: an application compacting its stored update log. Rows
// are one client's V1 updates in storage order — clocks 0-4, 5-6, 10-11 (a
// nested map "nested" holding key "k"), then 7-9.
var compactionRows = []string{
	"AQFlAAQBAXQFIGNlY2gA",
	"AQJlBQcBAWEBKABlBQF2AXcDaGJmAA==",
	"AQJlCicBAW0GbmVzdGVkASgAZQoBawF3BWdmYWhlAA==",
	"AQFlB8RlAWUCA2JmZwA=",
}

func readCompaction(t *testing.T, d *Doc) string {
	t.Helper()
	m, err := d.GetMap("m").ToJSON()
	require.NoError(t, err)
	return d.GetText("t").ToString() + " " + string(m)
}

const compactionWant = ` cbfgech {"nested":{"k":"gfahe"}}`

func TestUnit_ApplyUpdate_CompactedLogThenFiller(t *testing.T) {
	for _, f := range skipGapFormats {
		t.Run(f.name, func(t *testing.T) {
			rows := make([][]byte, len(compactionRows))
			for i, s := range compactionRows {
				v1, err := base64.StdEncoding.DecodeString(s)
				require.NoError(t, err)
				rows[i], err = f.conv(v1)
				require.NoError(t, err)
			}
			merged, err := f.merge(rows[0], rows[1], rows[2])
			require.NoError(t, err)
			d := New()
			require.NoError(t, f.apply(d, merged, nil))
			require.NoError(t, f.apply(d, rows[3], nil))
			require.Equal(t, compactionWant, readCompaction(t, d))
		})
	}
}

// EncodeStateAsUpdate must carry structs and deletions still parked on a
// missing dependency, as Yjs encodeStateAsUpdate includes pendingStructs and
// pendingDs. Otherwise a snapshot taken while anything is parked loses it for
// good once the filler arrives.
func TestUnit_EncodeStateAsUpdate_IncludesParked(t *testing.T) {
	encodes := []struct {
		name   string
		encode func(*Doc, StateVector) []byte
		apply  func(*Doc, []byte, any) error
	}{
		{"V1", EncodeStateAsUpdateV1, ApplyUpdateV1},
		{"V2", EncodeStateAsUpdateV2, ApplyUpdateV2},
	}
	rows := make([][]byte, len(compactionRows))
	for i, s := range compactionRows {
		var err error
		rows[i], err = base64.StdEncoding.DecodeString(s)
		require.NoError(t, err)
	}
	for _, e := range encodes {
		t.Run(e.name+"/structs", func(t *testing.T) {
			d := New()
			for _, r := range rows[:3] { // row 2 parks on the 7-9 gap
				require.NoError(t, ApplyUpdateV1(d, r, nil))
			}
			fresh := New()
			require.NoError(t, e.apply(fresh, e.encode(d, nil), nil))
			require.NoError(t, ApplyUpdateV1(fresh, rows[3], nil))
			require.Equal(t, compactionWant, readCompaction(t, fresh))
		})
		t.Run(e.name+"/structs-diff", func(t *testing.T) {
			d := New()
			for _, r := range rows[:3] {
				require.NoError(t, ApplyUpdateV1(d, r, nil))
			}
			peer := New()
			require.NoError(t, ApplyUpdateV1(peer, rows[0], nil))
			require.NoError(t, e.apply(peer, e.encode(d, peer.StateVector()), nil))
			require.NoError(t, ApplyUpdateV1(peer, rows[3], nil))
			require.Equal(t, compactionWant, readCompaction(t, peer))
		})
		t.Run(e.name+"/deletes", func(t *testing.T) {
			ups := threeUpdates(t, func(doc *Doc, i int) {
				doc.Transact(func(txn *Transaction) {
					txt := txn.GetText("t")
					switch i {
					case 0:
						txt.Insert(txn, 0, "abc", nil)
					case 1:
						txt.Insert(txn, 3, "d", nil)
					case 2:
						txt.Delete(txn, 3, 1)
					}
				})
			})
			d := New()
			require.NoError(t, ApplyUpdateV1(d, ups[0], nil))
			require.NoError(t, ApplyUpdateV1(d, ups[2], nil)) // delete of "d" parks
			fresh := New()
			require.NoError(t, e.apply(fresh, e.encode(d, nil), nil))
			require.NoError(t, ApplyUpdateV1(fresh, ups[1], nil))
			require.Equal(t, "abc", fresh.GetText("t").ToString())
		})
	}
}

// A struct can park over clocks the document's own structs hold. One parked
// behind a gap in its client's clocks still waits when an update fills the
// gap, if it also names a struct of a client the document lacks. One that
// arrives after those clocks parks too, as in Yjs, because the apply looks up
// what a struct names before it checks the struct's clocks. The apply never
// takes such clocks from the document's structs; it integrates only what a
// struct holds past them. Yjs's encodeStateAsUpdate keeps them too: its merge
// writes the state's run of a client's structs before the parked struct,
// which it then cuts. Merged whole, the parked struct takes those clocks,
// parks again in the reader, and what the document holds after them parks
// behind it.
func TestUnit_EncodeStateAsUpdate_ParkedOverHeldClocks(t *testing.T) {
	encodes := []struct {
		name   string
		encode func(*Doc, StateVector) []byte
		apply  func(*Doc, []byte, any) error
	}{
		{"V1", EncodeStateAsUpdateV1, ApplyUpdateV1},
		{"V2", EncodeStateAsUpdateV2, ApplyUpdateV2},
	}
	shapes := []struct {
		name    string
		clock   uint64
		text    string
		deleted DeleteRange
	}{
		{"over clocks the document holds", 5, "ABCDEFG", DeleteRange{Clock: 6, Len: 4}},
		{"over part of a struct the document holds", 4, "WXYZ", DeleteRange{Clock: 5, Len: 2}},
	}
	for _, e := range encodes {
		for _, s := range shapes {
			for _, first := range []bool{true, false} {
				name := e.name + "/" + s.name + "/struct after the text"
				if first {
					name = e.name + "/" + s.name + "/struct before the text"
				}
				t.Run(name, func(t *testing.T) {
					doc, waitedOn := parkedOverHeldClocks(t, s.clock, s.text, s.deleted, first)
					fresh := New(WithClientID(2))
					require.NoError(t, e.apply(fresh, e.encode(doc, nil), nil))
					require.Equal(t, doc.StateVector(), fresh.StateVector())
					require.Equal(t, doc.GetText("t").ToString(), fresh.GetText("t").ToString())
					require.Equal(t, parkedPastClocks(doc), parkedPastClocks(fresh))

					// Given the struct the parked one waits on, both hold the same.
					require.NoError(t, ApplyUpdateV1(doc, waitedOn, nil))
					require.NoError(t, ApplyUpdateV1(fresh, waitedOn, nil))
					require.Equal(t, doc.GetText("t").ToString(), fresh.GetText("t").ToString())
					require.Zero(t, doc.PendingStats().Items)
					require.Zero(t, fresh.PendingStats().Items)
				})
			}
		}
	}
}

// parkedOverHeldClocks returns a document that holds client 5's text
// "0123456789", with the clocks in deleted deleted, and its own text after it.
// It also holds a struct of client 5 parked over clocks it holds: text at
// clock, whose right origin is client 77's first clock. When first, that
// struct arrives before the text and parks behind the gap, and it still waits
// on client 77 once the text fills the gap; otherwise it arrives after the
// text and parks for client 77. The update of client 77's struct is returned
// too.
func parkedOverHeldClocks(t *testing.T, clock uint64, text string, deleted DeleteRange, first bool) (*Doc, []byte) {
	t.Helper()
	writer := New(WithClientID(5))
	written := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { written.Insert(txn, 0, "0123456789", nil) })
	over := waitingStruct(5, clock, text, deleted)
	doc := New(WithClientID(1))
	state := EncodeStateAsUpdateV1(writer, nil)
	if first {
		require.NoError(t, ApplyUpdateV1(doc, over, nil))
		require.NoError(t, ApplyUpdateV1(doc, state, nil))
	} else {
		require.NoError(t, ApplyUpdateV1(doc, state, nil))
		require.NoError(t, ApplyUpdateV1(doc, over, nil))
	}
	own := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { own.Insert(txn, own.Len(), "own", nil) })
	require.Equal(t, 1, doc.PendingStats().Items, "the document parks the struct")
	require.Equal(t, uint64(10), doc.StateVector()[5], "the document holds client 5's text")
	return doc, waitedOn()
}

// waitedOn is the update of client 77's first clock, a character of text "t",
// which a waitingStruct names as its right origin.
func waitedOn() []byte {
	waiter := New(WithClientID(77))
	waited := waiter.GetText("t")
	waiter.Transact(func(txn *Transaction) { waited.Insert(txn, 0, "R", nil) })
	return EncodeStateAsUpdateV1(waiter, nil)
}

// waitingStruct is an update of one text struct of client at clock whose right
// origin is client 77's first clock, and of the deletions deleted of client.
func waitingStruct(client ClientID, clock uint64, text string, deleted ...DeleteRange) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1) // one struct group
		enc.WriteVarUint(1) // of one struct
		enc.WriteVarUint(uint64(client))
		enc.WriteVarUint(clock)
		enc.WriteUint8(wireString | flagHasRightOrigin)
		enc.WriteVarUint(77) // right origin
		enc.WriteVarUint(0)
		enc.WriteVarString(text)
		if len(deleted) == 0 {
			enc.WriteVarUint(0) // no deletions
			return
		}
		enc.WriteVarUint(1) // one delete-set client
		enc.WriteVarUint(uint64(client))
		enc.WriteVarUint(uint64(len(deleted)))
		for _, d := range deleted {
			enc.WriteVarUint(d.Clock)
			enc.WriteVarUint(d.Len)
		}
	})
}

// A store that keeps each update a document took merges them to read the
// document back. One of them can carry a struct over clocks that earlier ones
// carry: here a struct of the typing client at its clock 3, 1,000 characters
// long, whose right origin no update carries, or a GC struct of those clocks.
// A document that applies the updates in order holds every keystroke and
// parks or collects only the struct's clocks past them. The merge reads back
// as that document, in V1 and V2, and so does that document's encoded state,
// also when the client sends its whole document again after the struct. Once
// the right origin arrives, each holds the same text as that document.
func TestUnit_MergeUpdates_ParkedOverHeldClocksReadsBackWhole(t *testing.T) {
	typist := New(WithClientID(1000))
	var typed [][]byte
	typist.OnUpdate(func(u []byte, _ any) { typed = append(typed, slices.Clone(u)) })
	text := typist.GetText("t")
	for i := range 1000 {
		typist.Transact(func(txn *Transaction) { text.Insert(txn, i/2, string(rune('a'+i%26)), nil) })
	}
	stored := append(slices.Clone(typed), waitingStruct(1000, 3, strings.Repeat("X", 1000)))
	collected := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1) // one struct group
		enc.WriteVarUint(1) // of one struct
		enc.WriteVarUint(1000)
		enc.WriteVarUint(3)
		enc.WriteUint8(0) // a GC struct
		enc.WriteVarUint(1000)
		enc.WriteVarUint(0) // no deletions
	})
	logs := []struct {
		name    string
		updates [][]byte
		parked  int
	}{
		{"keystrokes then the struct", stored, 1},
		{"keystrokes, the struct, then the whole document", append(slices.Clone(stored), EncodeStateAsUpdateV1(typist, nil)), 1},
		{"keystrokes then a GC struct", append(slices.Clone(typed), collected), 0},
	}
	for _, log := range logs {
		t.Run(log.name, func(t *testing.T) {
			want := New(WithClientID(2))
			for _, u := range log.updates {
				require.NoError(t, ApplyUpdateV1(want, u, nil))
			}
			require.Equal(t, text.ToString(), want.GetText("t").ToString(), "the document holds every keystroke")
			require.Equal(t, log.parked, want.PendingStats().Items, "the document parks the struct")

			v2 := make([][]byte, len(log.updates))
			for i, u := range log.updates {
				var err error
				v2[i], err = UpdateV1ToV2(u)
				require.NoError(t, err)
			}
			mergedV1, err := MergeUpdatesV1(log.updates...)
			require.NoError(t, err)
			mergedV2, err := MergeUpdatesV2(v2...)
			require.NoError(t, err)
			whole := New(WithClientID(4))
			for _, u := range append(slices.Clone(log.updates), waitedOn()) {
				require.NoError(t, ApplyUpdateV1(whole, u, nil))
			}
			reads := []struct {
				name   string
				update []byte
				apply  func(*Doc, []byte, any) error
			}{
				{"MergeUpdatesV1", mergedV1, ApplyUpdateV1},
				{"MergeUpdatesV2", mergedV2, ApplyUpdateV2},
				{"EncodeStateAsUpdateV1", EncodeStateAsUpdateV1(want, nil), ApplyUpdateV1},
			}
			for _, r := range reads {
				got := New(WithClientID(3))
				require.NoError(t, r.apply(got, r.update, nil), r.name)
				require.Equal(t, want.StateVector(), got.StateVector(), r.name)
				require.Equal(t, want.GetText("t").ToString(), got.GetText("t").ToString(), r.name)
				require.Equal(t, parkedPastClocks(want), parkedPastClocks(got), r.name)
				require.NoError(t, ApplyUpdateV1(got, waitedOn(), nil), r.name)
				require.Equal(t, whole.GetText("t").ToString(), got.GetText("t").ToString(), r.name)
			}
		})
	}
}

// parkedPastClocks is, per client, the clocks doc holds parked and not
// integrated. A parked struct's clocks below its client's clock are held by
// doc's own structs, and the apply never takes them from them.
func parkedPastClocks(doc *Doc) map[ClientID][]DeleteRange {
	doc.mu.Lock()
	defer doc.mu.Unlock()
	clocks := newDeleteSet()
	if doc.store.pending != nil {
		for _, it := range doc.store.pending.heldItems() {
			from := max(it.ID.Clock, doc.store.NextClock(it.ID.Client))
			if end := it.ID.Clock + it.Content.Len(); end > from {
				clocks.add(ID{Client: it.ID.Client, Clock: from}, end-from)
			}
		}
	}
	for c := range clocks.clients {
		clocks.sortAndCompact(c)
	}
	return clocks.clients
}
