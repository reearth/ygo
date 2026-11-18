package crdt

import (
	"strconv"
	"testing"
)

// manyClientsState is state with n more clients, ClientIDs from first on,
// each of which set one key of the map "m": a document a server keeps for
// long holds a client for every session that ever wrote to it. One
// transaction writes as each client in turn, so the build costs no
// transaction, and no copy of the state vector, per client.
func manyClientsState(tb testing.TB, state []byte, first ClientID, n int) []byte {
	tb.Helper()
	w := New(WithClientID(first))
	if err := ApplyUpdateV1(w, state, nil); err != nil {
		tb.Fatal(err)
	}
	m := w.GetMap("m")
	w.Transact(func(txn *Transaction) {
		for i := range n {
			w.setClientID(first + ClientID(i))
			m.Set(txn, "c"+strconv.Itoa(i), i)
		}
	})
	return EncodeStateAsUpdateV1(w, nil)
}

// BenchmarkApplyUpdateV1_ApplyCheckManyClients applies one keystroke a peer
// typed, as a remote update, to a document of 40,000 clients that has an
// apply check. Whether the apply is checked is asked of what the keystroke
// touched (updateLanded), and a drain with nothing parked is not checked
// (drainParked), so the check adds nothing in proportion to the 40,000
// clients.
func BenchmarkApplyUpdateV1_ApplyCheckManyClients(b *testing.B) {
	const clients = 40_000
	w := New(WithClientID(1))
	text := w.GetText("t")
	w.Transact(func(txn *Transaction) { text.Insert(txn, 0, "hello world", nil) })
	textState := EncodeStateAsUpdateV1(w, nil)

	doc := New(WithClientID(7))
	if err := ApplyUpdateV1(doc, manyClientsState(b, textState, 1000, clients-1), nil); err != nil {
		b.Fatal(err)
	}
	if got := len(doc.StateVector()); got != clients {
		b.Fatalf("the document holds %d clients, want %d", got, clients)
	}
	doc.SetApplyCheck(passEverything)

	// The peer holds only the text, so typing costs it nothing: each
	// keystroke is one struct of client 9, after the one before it.
	peer := New(WithClientID(9))
	if err := ApplyUpdateV1(peer, textState, nil); err != nil {
		b.Fatal(err)
	}
	typed := peer.GetText("t")
	keystrokes := make([][]byte, b.N)
	for i := range keystrokes {
		sv := peer.StateVector()
		peer.Transact(func(txn *Transaction) { typed.Insert(txn, typed.Len(), "x", nil) })
		keystrokes[i] = EncodeStateAsUpdateV1(peer, sv)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for _, keystroke := range keystrokes {
		if err := ApplyUpdateV1(doc, keystroke, nil); err != nil {
			b.Fatal(err)
		}
	}
}
