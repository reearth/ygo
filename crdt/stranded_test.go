package crdt_test

import (
	"testing"

	"github.com/reearth/ygo/crdt"
)

// TestUnit_Item_WriteIntoDeletedNestedTypeIsNotLive covers the paths the yjs
// fixtures cannot reach: yjs always collects, so a document with GC disabled is
// the only way a late write meets a parent that is deleted but still present.
func TestUnit_Item_WriteIntoDeletedNestedTypeIsNotLive(t *testing.T) {
	const writer = crdt.ClientID(3)
	for _, tc := range []struct {
		name    string
		gc      bool
		persist bool
	}{
		{"gc_single_doc", true, false},
		{"gc_persist_reload", true, true},
		{"no_gc_single_doc", false, false},
		{"no_gc_persist_reload", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := crdt.New(crdt.WithClientID(1))
			m := origin.GetMap("m")
			text := crdt.NewTextPrelim()
			origin.Transact(func(txn *crdt.Transaction) { m.Set(txn, "t", text) })
			origin.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "Hello", nil) })
			base := crdt.EncodeStateAsUpdateV1(origin, nil)

			deleter, deleterSV := strandedPeer(t, base, 2)
			deleterMap := deleter.GetMap("m")
			deleter.Transact(func(txn *crdt.Transaction) { deleterMap.Delete(txn, "t") })
			late, lateSV := strandedPeer(t, base, writer)
			lateValue, ok := late.GetMap("m").Get("t")
			if !ok {
				t.Fatal("writer is missing the nested text")
			}
			lateText := lateValue.(*crdt.YText)
			late.Transact(func(txn *crdt.Transaction) { lateText.Insert(txn, 5, "!", nil) })
			updates := [][]byte{
				crdt.EncodeStateAsUpdateV1(deleter, deleterSV),
				crdt.EncodeStateAsUpdateV1(late, lateSV),
			}

			var doc *crdt.Doc
			if tc.persist {
				state := base
				for _, u := range updates {
					doc = crdt.New(crdt.WithGC(tc.gc))
					mustApply(t, crdt.ApplyUpdateV1, doc, state)
					mustApply(t, crdt.ApplyUpdateV1, doc, u)
					state = crdt.EncodeStateAsUpdateV1(doc, nil)
				}
			} else {
				doc = crdt.New(crdt.WithGC(tc.gc))
				mustApply(t, crdt.ApplyUpdateV1, doc, base)
				for _, u := range updates {
					mustApply(t, crdt.ApplyUpdateV1, doc, u)
				}
			}
			if live := crdt.InsertSetFromDoc(doc, true); len(live.Ranges(writer)) != 0 {
				t.Errorf("write into the deleted text is live: %v", live.Ranges(writer))
			}
			if deleted := crdt.DeleteSetFromDoc(doc); len(deleted.Ranges(writer)) == 0 {
				t.Error("write into the deleted text is missing from the delete set")
			}
		})
	}
}

func strandedPeer(t *testing.T, base []byte, id crdt.ClientID) (*crdt.Doc, crdt.StateVector) {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(id))
	mustApply(t, crdt.ApplyUpdateV1, doc, base)
	sv, err := crdt.DecodeStateVectorV1(crdt.EncodeStateVectorV1(doc))
	if err != nil {
		t.Fatalf("state vector: %v", err)
	}
	return doc, sv
}
