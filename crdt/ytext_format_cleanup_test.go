package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func syncText(t *testing.T, from, to *Doc, origin any) {
	t.Helper()
	require.NoError(t, ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, to.StateVector()), origin))
}

// A empties the text while B bolds its last character. Once A's
// deletions reach B, B drops the emptied range's redundant opener, so text A
// inserts there later is plain on B, as in Yjs.
func TestUnit_YText_RemoteDeleteCleansFormatMarkers(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
	b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })
	require.Equal(t, 2, countLiveContentFormat(b))

	syncText(t, a, b, nil)
	assert.Equal(t, 1, countLiveContentFormat(b), "the emptied range keeps only its last marker")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "x", nil) })
	syncText(t, a, b, nil)
	assert.Equal(t, []Delta{{Op: DeltaOpInsert, Insert: "x"}}, bt.ToDelta())

	syncText(t, b, a, nil)
	assert.Equal(t, at.ToDelta(), bt.ToDelta())
	assert.Equal(t, a.StateVector(), b.StateVector())
}

// The cleanup runs after the remote transaction as its own local transaction
// with a nil origin, emitting the deletions as an update of its own (Yjs
// cleanupYTextAfterTransaction parity).
func TestUnit_YText_FormatCleanup_IsLocalFollowUpTxn(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
	b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })

	type txnInfo struct {
		origin any
		local  bool
	}
	var txns []txnInfo
	var updates [][]byte
	b.OnAfterTransaction(func(txn *Transaction) { txns = append(txns, txnInfo{txn.Origin, txn.Local}) })
	b.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	syncText(t, a, b, "remote")
	require.Equal(t, []txnInfo{{"remote", false}, {nil, true}}, txns)
	require.Len(t, updates, 2)

	// The cleanup update alone carries B's marker deletion to a peer.
	c := newTestDoc(3)
	syncText(t, b, c, nil)
	assert.Equal(t, bt.ToDelta(), c.GetText("t").ToDelta())
	assert.Equal(t, 1, countLiveContentFormat(c))
}

// Plain text and local edits never start a cleanup transaction.
func TestUnit_YText_FormatCleanup_SkipsPlainAndLocal(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	var n int
	b.OnAfterTransaction(func(*Transaction) { n++ })
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 2) })
	syncText(t, a, b, nil)
	assert.Equal(t, 2, n, "plain remote updates: one transaction each")

	b.Transact(func(txn *Transaction) {
		bt.Insert(txn, 1, "xy", Attributes{"bold": true})
		bt.Delete(txn, 0, 3)
	})
	assert.Equal(t, 3, n, "a local edit: one transaction")
}
