package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_DrainPending_UnchangedClientsNeedNoClockReads(t *testing.T) {
	const n = 1000
	doc := New(WithClientID(100001))
	defer doc.Destroy()
	work := resolverWork{}
	missing := make(StateVector, n)
	items := make([]*Item, n)
	for i := range n {
		client := ClientID(i + 1)
		doc.store.Append(&Item{ID: ID{Client: client}, Content: &resolverWorkContent{NewContentDeleted(1), &work}})
		missing[client] = 1
		items[i] = &Item{ID: ID{Client: client + n}, Origin: &ID{Client: client, Clock: 1}, Content: NewContentDeleted(1)}
	}
	doc.store.pending = &pendingUpdate{items: items, missing: missing}
	doc.Transact(func(txn *Transaction) {
		work.lengths = 0
		require.NoError(t, drainPending(txn))
		require.Zero(t, work.lengths, "a drain with no clock advance must not read waiting clients")
	})
	require.Equal(t, n, doc.PendingStats().Items)
}

func TestUnit_Retryable_FailedApplyThenEmptyKeepsClockAdvance(t *testing.T) {
	for _, c := range partlyAppliedCases(t) {
		t.Run(c.name, func(t *testing.T) {
			doc := New(WithClientID(1))
			defer doc.Destroy()
			require.NoError(t, c.apply(doc, c.gap))
			require.Equal(t, 1, doc.PendingStats().Items)
			require.ErrorIs(t, c.apply(doc, c.bad), ErrInvalidUpdate)
			require.Positive(t, doc.StateVector().Clock(13), "the failed apply must advance the dependency")
			empty := []byte{0, 0}
			if c.v2 {
				var err error
				empty, err = UpdateV1ToV2(empty)
				require.NoError(t, err)
			}
			require.NoError(t, c.apply(doc, empty))
			require.Equal(t, "ab", doc.GetText("t").ToString())
			require.Equal(t, uint64(2), doc.StateVector().Clock(13))
			require.Zero(t, doc.PendingStats().Items)
		})
	}
}

func TestUnit_Retryable_RefusedApplyKeepsAnEarlierWake(t *testing.T) {
	for _, c := range partlyAppliedCases(t) {
		t.Run(c.name, func(t *testing.T) {
			doc := New(WithClientID(13))
			defer doc.Destroy()
			text := doc.GetText("t")
			require.NoError(t, c.apply(doc, c.gap))
			doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, "a", nil) })
			require.Equal(t, 1, doc.PendingStats().Items)
			require.True(t, doc.store.retryable())
			require.ErrorIs(t, c.apply(doc, []byte{1}), ErrInvalidUpdate)
			require.Equal(t, 1, doc.PendingStats().Items, "a refused decode must not drain")
			require.True(t, doc.store.retryable(), "a refused decode must not consume the earlier wake")
			empty := []byte{0, 0}
			if c.v2 {
				var err error
				empty, err = UpdateV1ToV2(empty)
				require.NoError(t, err)
			}
			require.NoError(t, c.apply(doc, empty))
			require.Equal(t, "ab", text.ToString())
			require.Zero(t, doc.PendingStats().Items)
		})
	}
}
