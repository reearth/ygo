package crdt

import (
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
