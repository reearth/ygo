package crdt_test

import (
	"fmt"

	"github.com/reearth/ygo/crdt"
)

// Example_subdocs demonstrates embedding a Doc as a subdocument of another
// Doc and observing subdocument lifecycle events. Embedding is done by
// setting a *crdt.Doc as a YMap value; OnSubdocs reports which subdocuments
// were added, removed, or (re)loaded during a transaction.
func Example_subdocs() {
	parent := crdt.New()
	parent.OnSubdocs(func(ev crdt.SubdocsEvent) {
		fmt.Printf("added=%d loaded=%d\n", len(ev.Added), len(ev.Loaded))
	})

	child := crdt.New(crdt.WithGUID("child-1"))
	root := parent.GetMap("root")
	parent.Transact(func(txn *crdt.Transaction) {
		root.Set(txn, "doc", child)
	})

	fmt.Println("subdocs:", parent.GetSubdocGUIDs())
	// Output:
	// added=1 loaded=1
	// subdocs: [child-1]
}
