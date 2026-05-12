package crdt_test

import (
	"fmt"

	"github.com/reearth/ygo/crdt"
)

// ExampleDoc_Transact shows the basic pattern: batch mutations in a
// transaction, observe them via OnUpdate.
func ExampleDoc_Transact() {
	doc := crdt.New()
	text := doc.GetText("body")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "hello", nil)
		text.Insert(txn, 5, " world", nil)
	})
	fmt.Println(text.ToString())
	// Output: hello world
}

// ExampleDoc_OnUpdate shows how to observe committed transactions.
// The callback fires once per transaction with the V1-encoded update.
func ExampleDoc_OnUpdate() {
	doc := crdt.New()
	arr := doc.GetArray("items")

	updates := 0
	unsub := doc.OnUpdate(func(update []byte, origin any) {
		updates++
	})
	defer unsub()

	doc.Transact(func(txn *crdt.Transaction) {
		arr.Insert(txn, 0, []any{"a", "b", "c"})
	})

	fmt.Println("updates fired:", updates)
	// Output: updates fired: 1
}

// ExampleYArray_Insert demonstrates inserting and reading array items.
func ExampleYArray_Insert() {
	doc := crdt.New()
	arr := doc.GetArray("list")
	doc.Transact(func(txn *crdt.Transaction) {
		arr.Insert(txn, 0, []any{"first", "third"})
		arr.Insert(txn, 1, []any{"second"})
	})

	for i := 0; i < arr.Len(); i++ {
		fmt.Println(arr.Get(i))
	}
	// Output:
	// first
	// second
	// third
}

// ExampleYMap_Set demonstrates map key operations.
func ExampleYMap_Set() {
	doc := crdt.New()
	m := doc.GetMap("config")
	doc.Transact(func(txn *crdt.Transaction) {
		m.Set(txn, "theme", "dark")
		m.Set(txn, "fontSize", "14")
	})

	v, ok := m.Get("theme")
	fmt.Println("theme:", v, ok)
	// Output: theme: dark true
}

// ExampleYText_Insert shows inserting plain text into a YText type.
func ExampleYText_Insert() {
	doc := crdt.New()
	text := doc.GetText("doc")
	doc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "Hello, ", nil)
		text.Insert(txn, 7, "world!", nil)
	})
	fmt.Println(text.ToString())
	// Output: Hello, world!
}

// ExampleApplyUpdateV1 demonstrates peer-to-peer sync. Peer A produces
// an update; Peer B applies it and converges to the same state.
func ExampleApplyUpdateV1() {
	peerA := crdt.New()
	textA := peerA.GetText("doc")
	peerA.Transact(func(txn *crdt.Transaction) {
		textA.Insert(txn, 0, "from A", nil)
	})
	update := crdt.EncodeStateAsUpdateV1(peerA, nil)

	peerB := crdt.New()
	if err := crdt.ApplyUpdateV1(peerB, update, nil); err != nil {
		fmt.Println("apply error:", err)
		return
	}
	textB := peerB.GetText("doc")
	fmt.Println(textB.ToString())
	// Output: from A
}
