package crdt_test

import (
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// The transaction-scoped accessors are valid only inside the Transact
// callback that received the Transaction. The two realistic misuses both
// happen on the SAME goroutine that ran the transaction — an observer
// (OnAfterTransaction receives the *Transaction after the lock is released)
// or code that retained the txn past the callback — so a committed-flag
// check is deterministic there: it must PANIC, not silently mutate d.share
// without the lock (a data race against concurrent doc.Get* callers).

// TestIssue138_TxnAccessorPanicsInObserver: OnAfterTransaction hands the
// observer the *Transaction after unlock; txn.Get* there must fail loudly.
func TestIssue138_TxnAccessorPanicsInObserver(t *testing.T) {
	doc := crdt.New()
	txt := doc.GetText("t")

	var rec any
	unsub := doc.OnAfterTransaction(func(txn *crdt.Transaction) {
		defer func() { rec = recover() }()
		_ = txn.GetText("other") // would create-on-miss in d.share, unlocked
	})
	defer unsub()

	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "x", nil)
	}, nil)

	if rec == nil {
		t.Fatal("txn.GetText inside OnAfterTransaction must panic (transaction already committed)")
	}
	if msg, ok := rec.(string); !ok || !strings.Contains(msg, "after commit") {
		t.Fatalf("panic should say the transaction is used after commit, got: %v", rec)
	}
}

// TestIssue138_TxnAccessorPanicsWhenRetained: a *Transaction captured out of
// the callback must fail loudly on accessor use after Transact returns.
func TestIssue138_TxnAccessorPanicsWhenRetained(t *testing.T) {
	doc := crdt.New()
	var stale *crdt.Transaction
	doc.Transact(func(txn *crdt.Transaction) { stale = txn }, nil)

	var rec any
	func() {
		defer func() { rec = recover() }()
		_ = stale.GetMap("m")
	}()
	if rec == nil {
		t.Fatal("retained txn.GetMap after Transact returned must panic")
	}
}
