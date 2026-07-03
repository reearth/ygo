package crdt_test

import (
	"fmt"

	"github.com/reearth/ygo/crdt"
)

// Example_attribution stamps an incoming update with authorship metadata and
// reads it back — the y/hub-style server flow (issue #56).
func Example_attribution() {
	// A collaborator produced an update…
	src := crdt.New(crdt.WithClientID(7))
	txt := src.GetText("t") // resolve OUTSIDE Transact
	src.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })
	update := crdt.EncodeStateAsUpdateV1(src, nil)

	// …the server stamps it without integrating it.
	ids, _ := crdt.ContentIDsFromUpdateV1(update)
	userid := crdt.MustContentAttribute("userid", "alice")
	cm := crdt.CreateContentMapFromContentIDs(ids, []*crdt.ContentAttribute{userid}, nil)

	// Store EncodeContentMap(cm) next to the update; later, read it back.
	decoded, _ := crdt.DecodeContentMap(crdt.EncodeContentMap(cm))
	for _, r := range decoded.Inserts.Slice(7, 0, 2) {
		fmt.Printf("clocks [%d,%d): %s=%v\n", r.Clock, r.Clock+r.Len, r.Attrs[0].Name, r.Attrs[0].Value)
	}
	// Output: clocks [0,2): userid=alice
}
