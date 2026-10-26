package crdt

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// OnUpdate observers run after the transaction releases the document lock, so
// a transaction that commits while an earlier transaction's observer is still
// running has its observer run first: OnUpdate alone does not give commit
// order. This is why UpdateLog exists; the test needs only OnUpdate.
func TestUnit_OnUpdate_ObserversRunOutOfCommitOrder(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")

	firstObserving := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var observed []any
	doc.OnUpdate(func(_ []byte, origin any) {
		if origin == "first" {
			close(firstObserving)
			<-releaseFirst
		}
		mu.Lock()
		observed = append(observed, origin)
		mu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "first", nil) }, "first")
	}()
	// The first transaction has committed: its observer is running.
	<-firstObserving
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "second ", nil) }, "second")
	close(releaseFirst)
	<-done
	require.Equal(t, "second first", txt.ToString(), "first committed, then second")
	require.Equal(t, []any{"second", "first"}, observed,
		"the second commit's observer ran before the first's")
}
