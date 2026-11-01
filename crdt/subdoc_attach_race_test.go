package crdt

import (
	"sync"
	"testing"
	"time"
)

// TestRace_SubdocumentAttachAndLoad starts attachment and Load together. The
// child observes its item and parent as one pair, and integration observes a
// stable shouldLoad value.
func TestRace_SubdocumentAttachAndLoad(t *testing.T) {
	for range 500 {
		parent := New(WithClientID(1))
		root := parent.GetMap("root")
		child := New(WithShouldLoad(false))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			parent.Transact(func(txn *Transaction) { root.Set(txn, "child", child) })
		}()
		go func() {
			defer wg.Done()
			<-start
			child.Load()
		}()
		close(start)
		wg.Wait()
	}
}

// A document embedded in itself attaches and detaches inside its own
// transaction and returns, as in Yjs.
func TestUnit_Subdoc_SelfEmbeddingReturns(t *testing.T) {
	doc := New(WithClientID(1))
	m := doc.GetMap("m")
	done := make(chan struct{})
	go func() {
		defer close(done)
		doc.Transact(func(txn *Transaction) { m.Set(txn, "self", doc) })
		doc.Transact(func(txn *Transaction) { m.Delete(txn, "self") })
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("embedding a document in itself did not return")
	}
}
