package crdt

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// deleteSetUpdate returns a V1 update that carries only a delete set of one
// range of client.
func deleteSetUpdate(client ClientID, clock uint64, length int) []byte {
	ds := newDeleteSet()
	ds.add(ID{Client: client, Clock: clock}, length)
	return encodeStructStoreV1(nil, ds, nil, newStructStore())
}

// Client 1 holds a deleted run at clocks 0-7 and the live "ij" at 8-9; a delete
// set over part of the run leaves it whole, as Yjs readAndApplyDeleteSet does.
func TestUnit_DeleteSet_DeletedItemStaysWhole(t *testing.T) {
	t.Parallel()
	for _, gc := range []bool{true, false} {
		for _, tc := range []struct {
			name      string
			clock     uint64
			length    int
			wantItems int
			wantText  string
		}{
			{"inside the deleted run", 2, 3, 2, "ij"},
			{"from the deleted run into a live item", 6, 3, 3, "j"},
			{"exactly the deleted run", 0, 8, 2, "ij"},
		} {
			t.Run(fmt.Sprintf("%s/gc=%t", tc.name, gc), func(t *testing.T) {
				t.Parallel()
				doc := New(WithClientID(1), WithGC(gc))
				txt := doc.GetText("t")
				doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcdefgh", nil) })
				doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 8) })
				doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ij", nil) })
				require.Len(t, doc.store.clients[1], 2)

				update := deleteSetUpdate(1, tc.clock, tc.length)
				var during int
				doc.Transact(func(txn *Transaction) {
					txn.Local = false
					require.NoError(t, applyV1Txn(txn, update))
					during = len(doc.store.clients[1])
				})
				require.Equal(t, tc.wantItems, during, "items before the commit merges any")
				require.Len(t, doc.store.clients[1], tc.wantItems)
				require.Equal(t, tc.wantText, txt.ToString())
				require.Empty(t, doc.store.pendingDs.clients)
			})
		}
	}
}

// A delete set that deletes nothing new leaves the document's state as it was,
// also for a deleted item with a redone link, whose halves cannot merge back.
func TestUnit_DeleteSet_RedoneDeletedItemStaysWhole(t *testing.T) {
	t.Parallel()
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	um := NewUndoManager(doc, []SharedType{txt})
	defer um.Destroy()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcdefgh", nil) })
	um.StopCapturing()
	doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 8) })
	require.True(t, um.Undo())
	require.Equal(t, "abcdefgh", txt.ToString())
	require.NotNil(t, doc.store.clients[1][0].redone, "undo restores the run as a redone copy")

	before := EncodeStateAsUpdateV1(doc, nil)
	require.NoError(t, ApplyUpdateV1(doc, deleteSetUpdate(1, 2, 3), nil))
	require.Equal(t, before, EncodeStateAsUpdateV1(doc, nil))
	require.Equal(t, "abcdefgh", txt.ToString())
}

// A delete set that starts or ends inside a deleted supplementary character
// (clocks 1-2 of "x😀y") leaves it whole, so undo restores the character
// instead of two replacement characters.
func TestUnit_DeleteSet_DeletedSurrogatePairStaysWhole(t *testing.T) {
	t.Parallel()
	for _, clock := range []uint64{1, 2} {
		t.Run(fmt.Sprintf("clock=%d", clock), func(t *testing.T) {
			t.Parallel()
			doc := newTestDoc(1)
			txt := doc.GetText("t")
			um := NewUndoManager(doc, []SharedType{txt})
			defer um.Destroy()
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "x😀y", nil) })
			um.StopCapturing()
			doc.Transact(func(txn *Transaction) { txt.Delete(txn, 0, txt.Len()) })

			require.NoError(t, ApplyUpdateV1(doc, deleteSetUpdate(1, clock, 1), nil))
			require.True(t, um.Undo())
			require.Equal(t, "x😀y", txt.ToString())
		})
	}
}
