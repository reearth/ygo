package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// YArray.Insert and Push with no values make no item, as in Yjs: the document's
// state is the state it has without the call, so a fresh peer reads what the
// writer encodes.
func TestUnit_YArray_InsertOrPushOfNoValuesMakesNoItem(t *testing.T) {
	cases := map[string]struct {
		before func(*Transaction, *YArray, *YText)
		empty  func(*Transaction, *YArray)
	}{
		"Insert at the end": {
			before: func(txn *Transaction, a *YArray, _ *YText) { a.Insert(txn, 0, []any{"x", "y"}) },
			empty:  func(txn *Transaction, a *YArray) { a.Insert(txn, 2, []any{}) },
		},
		"Insert inside an item": {
			before: func(txn *Transaction, a *YArray, _ *YText) { a.Insert(txn, 0, []any{"x", "y"}) },
			empty:  func(txn *Transaction, a *YArray) { a.Insert(txn, 1, nil) },
		},
		"Insert into an empty array after other content": {
			before: func(txn *Transaction, _ *YArray, txt *YText) { txt.Insert(txn, 0, "ab", nil) },
			empty:  func(txn *Transaction, a *YArray) { a.Insert(txn, 0, []any{}) },
		},
		"Push": {
			before: func(txn *Transaction, a *YArray, _ *YText) { a.Push(txn, []any{"x"}) },
			empty:  func(txn *Transaction, a *YArray) { a.Push(txn, []any{}) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			want := New(WithClientID(7))
			wantArr, wantTxt := want.GetArray("a"), want.GetText("t")
			want.Transact(func(txn *Transaction) { tc.before(txn, wantArr, wantTxt) })

			for _, sameTxn := range []bool{false, true} {
				got := New(WithClientID(7))
				arr, txt := got.GetArray("a"), got.GetText("t")
				if sameTxn {
					got.Transact(func(txn *Transaction) { tc.before(txn, arr, txt); tc.empty(txn, arr) })
				} else {
					got.Transact(func(txn *Transaction) { tc.before(txn, arr, txt) })
					got.Transact(func(txn *Transaction) { tc.empty(txn, arr) })
				}

				state := EncodeStateAsUpdateV1(got, nil)
				require.Equal(t, EncodeStateAsUpdateV1(want, nil), state, "V1 state, same transaction %v", sameTxn)
				require.Equal(t, EncodeStateAsUpdateV2(want, nil), EncodeStateAsUpdateV2(got, nil), "V2 state, same transaction %v", sameTxn)
				peer := New(WithClientID(8))
				require.NoError(t, ApplyUpdateV1(peer, state, nil))
				assert.Equal(t, arr.ToSlice(), peer.GetArray("a").ToSlice())
			}
		})
	}
}
