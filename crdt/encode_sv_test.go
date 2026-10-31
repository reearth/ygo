package crdt

import (
	"math/bits"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fragmentedText returns a doc whose text "t" was typed as "ab" repeated n
// times by client 1, after which every "b" was deleted in its own
// transaction, so client 1 holds 2n items.
func fragmentedText(n int) (*Doc, *YText) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, strings.Repeat("ab", n), nil) })
	for i := n - 1; i >= 0; i-- {
		doc.Transact(func(txn *Transaction) { txt.Delete(txn, 2*i+1, 1) })
	}
	return doc, txt
}

// lenCounter counts calls to Len on the content it wraps.
type lenCounter struct {
	Content
	calls *int
}

func (c lenCounter) Len() int {
	*c.calls++
	return c.Content.Len()
}

func TestUnit_EncodeStateAsUpdate_SkipsItemsBeforeStateVector(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		encode func(*Doc, StateVector) []byte
	}{{"V1", EncodeStateAsUpdateV1}, {"V2", EncodeStateAsUpdateV2}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, txt := fragmentedText(1000)
			sv := doc.StateVector()
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "x", nil) })

			// Count Len calls on every item the peer already has that is
			// not deleted (the delete set reads the length of deleted ones).
			calls := 0
			items := doc.store.clients[1]
			for _, it := range items[:len(items)-1] {
				if !it.Deleted {
					it.Content = lenCounter{it.Content, &calls}
				}
			}
			tc.encode(doc, sv)
			require.LessOrEqual(t, calls, bits.Len(uint(len(items)))+1,
				"encoding against a state vector reads the items before it")
		})
	}
}

func TestUnit_EncodeStateAsUpdate_StateVectorMatchesDiffUpdate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		encode func(*Doc, StateVector) []byte
		diff   func([]byte, StateVector) ([]byte, error)
		apply  func(*Doc, []byte, any) error
	}{
		{"V1", EncodeStateAsUpdateV1, DiffUpdateV1, ApplyUpdateV1},
		{"V2", EncodeStateAsUpdateV2, DiffUpdateV2, ApplyUpdateV2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Two clients with multi-character items, so state vector clocks
			// fall before, inside and after items.
			doc, txt := fragmentedText(20)
			peer := newTestDoc(2)
			require.NoError(t, ApplyUpdateV1(peer, EncodeStateAsUpdateV1(doc, nil), nil))
			peerTxt := peer.GetText("t")
			peer.Transact(func(txn *Transaction) { peerTxt.Insert(txn, 3, "hello", nil) })
			peer.Transact(func(txn *Transaction) { peerTxt.Insert(txn, 0, "world", nil) })
			require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(peer, doc.StateVector()), nil))
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "xyz", nil) })

			full := tc.encode(doc, nil)
			replica := newTestDoc(3)
			require.NoError(t, tc.apply(replica, full, nil))
			require.Equal(t, txt.ToString(), replica.GetText("t").ToString())
			end := doc.StateVector()
			for c1 := uint64(0); c1 <= end[1]+1; c1++ {
				for _, c2 := range []uint64{0, 2, 5, 7, end[2], end[2] + 1} {
					sv := StateVector{1: c1, 2: c2}
					want, err := tc.diff(full, sv)
					require.NoError(t, err)
					require.Equal(t, want, tc.encode(doc, sv), "sv %v", sv)
				}
			}
		})
	}
}
