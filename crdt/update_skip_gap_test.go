package crdt

import (
	"encoding/base64"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
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
