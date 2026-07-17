package crdt_test

import (
	"sync"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// Issue #138: doc.GetMap / GetText / GetArray / GetXmlFragment take the
// document lock, so calling one inside a Transact callback (which already
// holds that lock) self-deadlocks a non-reentrant mutex.
//
// Resolution: transaction-scoped accessors. Inside a Transact callback,
// resolve root types through the Transaction — txn.GetText / txn.GetMap /
// txn.GetArray / txn.GetXmlFragment reuse the lock the transaction already
// holds, so the natural in-transaction call simply works (yrs ergonomics).
// The Doc-level accessors remain lock-taking and must still be called
// OUTSIDE transactions; misusing them inside a callback deadlocks — Go has
// no goroutine identity, so that misuse is documented rather than detected
// (a deliberately hanging test would hang the suite, so there is none).

// TestIssue138_TxnAccessorsWorkInsideTransact pins the fix: resolving and
// mutating root types entirely inside the callback via the Transaction
// accessors.
func TestIssue138_TxnAccessorsWorkInsideTransact(t *testing.T) {
	doc := crdt.New()
	doc.Transact(func(txn *crdt.Transaction) {
		txn.GetText("t").Insert(txn, 0, "hi", nil)
		txn.GetMap("m").Set(txn, "k", int64(42))
		txn.GetArray("a").Insert(txn, 0, []any{int64(1)})
		txn.GetXmlFragment("x").InsertElement(txn, 0, crdt.NewYXmlElement("p"))
	}, nil)

	if got := doc.GetText("t").ToString(); got != "hi" {
		t.Errorf("text = %q, want %q", got, "hi")
	}
	if v, ok := doc.GetMap("m").Get("k"); !ok || v != int64(42) {
		t.Errorf("map k = %v, %v; want 42, true", v, ok)
	}
	if got := doc.GetArray("a").Len(); got != 1 {
		t.Errorf("array len = %d, want 1", got)
	}
	if got := doc.GetXmlFragment("x").Len(); got != 1 {
		t.Errorf("fragment len = %d, want 1", got)
	}
}

// TestIssue138_TxnAccessorSameHandleAsDoc proves txn and Doc accessors
// resolve the SAME shared type instance for the same name.
func TestIssue138_TxnAccessorSameHandleAsDoc(t *testing.T) {
	doc := crdt.New()
	outside := doc.GetText("t")
	var inside *crdt.YText
	doc.Transact(func(txn *crdt.Transaction) {
		inside = txn.GetText("t")
		inside.Insert(txn, 0, "x", nil)
	}, nil)
	if inside != outside {
		t.Fatalf("txn.GetText returned a different instance than doc.GetText")
	}
	if got := outside.ToString(); got != "x" {
		t.Fatalf("text = %q, want %q", got, "x")
	}
}

// TestIssue138_ResolveBeforeTransactStillWorks pins the other documented
// idiom — resolve handles first, mutate inside.
func TestIssue138_ResolveBeforeTransactStillWorks(t *testing.T) {
	doc := crdt.New()
	m := doc.GetMap("m")
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) {
		m.Set(txn, "k", int64(42))
		txt.Insert(txn, 0, "hi", nil)
	}, nil)
	if v, ok := m.Get("k"); !ok || v != int64(42) {
		t.Fatalf("map value = %v, %v; want 42, true", v, ok)
	}
	if s := txt.ToString(); s != "hi" {
		t.Fatalf("text = %q; want %q", s, "hi")
	}
}

// TestIssue138_ConcurrentAccessorDuringTransactBlocks pins the
// cross-goroutine contract: a Doc-level accessor called while another
// goroutine's transaction holds the lock completes only AFTER that
// transaction commits, and never panics (ApplyUpdate wraps an entire sync
// in one Transact, so arbitrarily long holds are legitimate). Deterministic
// — ordering is proven by a flag written under the lock as the callback's
// last act (the mutex Unlock→Lock handoff is the happens-before edge that
// makes the read safe), not by wall-clock.
func TestIssue138_ConcurrentAccessorDuringTransactBlocks(t *testing.T) {
	doc := crdt.New()
	inTxn := make(chan struct{})
	proceed := make(chan struct{})
	committed := false // written under d.mu inside the callback

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		doc.Transact(func(txn *crdt.Transaction) {
			close(inTxn) // the lock is held from before this point...
			<-proceed    // ...and stays held until the accessor call is in flight
			committed = true
		}, nil)
	}()

	<-inTxn // the transaction owns the lock now
	var rec any
	var m *crdt.YMap
	go close(proceed) // release strictly concurrently with the call below
	func() {
		defer func() { rec = recover() }()
		m = doc.GetMap("m")
	}()
	wg.Wait()

	if rec != nil {
		t.Fatalf("concurrent GetMap must block until commit, not panic; got: %v", rec)
	}
	if m == nil {
		t.Fatal("GetMap returned nil")
	}
	// GetMap can only return after acquiring d.mu; committed is the
	// callback's final write under that lock. Seeing false here would mean
	// the accessor completed while the transaction was still open — i.e. it
	// jumped the lock.
	if !committed {
		t.Fatal("GetMap returned while the transaction was still open (lock was not respected)")
	}
}
