package crdt

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// onUpdateDeleteSet runs fn on doc and returns the delete set of the update
// its OnUpdate observer receives, per client. Decoding sorts and merges the
// ranges, so it also requires the update to end with them written that way.
func onUpdateDeleteSet(t *testing.T, doc *Doc, fn func()) map[ClientID][]DeleteRange {
	t.Helper()
	var update []byte
	unsub := doc.OnUpdate(func(u []byte, _ any) { update = u })
	defer unsub()
	fn()
	require.NotNil(t, update, "OnUpdate did not fire")
	_, ds, err := decodeStructsV1(New(), update)
	require.NoError(t, err)
	written := encoding.EncodeBytes(func(enc *encoding.Encoder) { encodeDeleteSet(enc, ds) })
	require.True(t, bytes.HasSuffix(update, written), "delete set not written sorted and merged: %x", update)
	return ds.clients
}

// deletionHistoryDoc returns client 1's text "acefghij" (clocks 0-9, with b at
// 1 and d at 3 deleted) and map m with k overwritten (clock 10 deleted, 11
// live), each edit in its own transaction.
func deletionHistoryDoc(t *testing.T) (*Doc, *YText, *YMap) {
	t.Helper()
	doc := newTestDoc(1)
	txt, m := doc.GetText("t"), doc.GetMap("m")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcdefghij", nil) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 1, 1) })
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 2, 1) })
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", float64(1)) })
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", float64(2)) })
	require.Equal(t, "acefghij", txt.ToString())
	return doc, txt, m
}

func TestUnit_DocOnUpdate_CarriesOnlyTheTransactionsDeletions(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(txn *Transaction, txt *YText, m *YMap)
		want map[ClientID][]DeleteRange
	}{
		{
			name: "keystroke",
			edit: func(txn *Transaction, txt *YText, _ *YMap) { txt.Insert(txn, 8, "x", nil) },
			want: map[ClientID][]DeleteRange{},
		},
		{
			name: "one deletion",
			edit: func(txn *Transaction, txt *YText, _ *YMap) { txt.Delete(txn, 3, 1) },
			want: map[ClientID][]DeleteRange{1: {{Clock: 5, Len: 1}}},
		},
		{
			name: "two deletions, the later one first",
			edit: func(txn *Transaction, txt *YText, _ *YMap) {
				txt.Delete(txn, 6, 1)
				txt.Delete(txn, 3, 1)
			},
			want: map[ClientID][]DeleteRange{1: {{Clock: 5, Len: 1}, {Clock: 8, Len: 1}}},
		},
		{
			name: "adjacent deletions, the later one first",
			edit: func(txn *Transaction, txt *YText, _ *YMap) {
				txt.Delete(txn, 4, 1)
				txt.Delete(txn, 3, 1)
			},
			want: map[ClientID][]DeleteRange{1: {{Clock: 5, Len: 2}}},
		},
		{
			name: "map overwrite",
			edit: func(txn *Transaction, _ *YText, m *YMap) { m.Set(txn, "k", float64(3)) },
			want: map[ClientID][]DeleteRange{1: {{Clock: 11, Len: 1}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, txt, m := deletionHistoryDoc(t)
			got := onUpdateDeleteSet(t, doc, func() {
				doc.Transact(func(txn *Transaction) { tc.edit(txn, txt, m) })
			})
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUnit_DocOnUpdate_RemoteApplyCarriesOnlyItsDeletions(t *testing.T) {
	for _, f := range []struct {
		name   string
		encode func(*Doc, StateVector) []byte
		apply  func(*Doc, []byte, any) error
	}{
		{"V1", EncodeStateAsUpdateV1, ApplyUpdateV1},
		{"V2", EncodeStateAsUpdateV2, ApplyUpdateV2},
	} {
		t.Run(f.name, func(t *testing.T) {
			author, authorTxt, _ := deletionHistoryDoc(t)
			peer := newTestDoc(2)
			require.NoError(t, f.apply(peer, f.encode(author, nil), nil))
			peerTxt := peer.GetText("t")
			// The peer deletes h (clock 7), the author j (clock 9).
			peer.Transact(func(txn *Transaction) { peerTxt.Delete(txn, 5, 1) })
			author.Transact(func(txn *Transaction) { authorTxt.Delete(txn, 7, 1) })

			// The author's update carries its whole delete set; the peer
			// lacks only the deletion of j.
			update := f.encode(author, peer.StateVector())
			got := onUpdateDeleteSet(t, peer, func() { require.NoError(t, f.apply(peer, update, nil)) })
			require.Equal(t, map[ClientID][]DeleteRange{1: {{Clock: 9, Len: 1}}}, got)
			require.Equal(t, "acefgi", peerTxt.ToString())
		})
	}
}
