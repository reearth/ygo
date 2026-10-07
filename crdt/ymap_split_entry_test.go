package crdt

import (
	"encoding/hex"
	"fmt"
	"testing"
)

// splitEntryBase is Yjs's map_three_values_nogc fixture: key k is one deleted
// run (10,0..2) of three values.
const splitEntryBase = "01010a002801016d016b037d017d027d03010a010003"

// TestSplitItem_KeyTracksRightHalf: splitting a key's run mid-transaction
// moves itemMap to the right half, and re-merging the halves at commit moves
// it back, so the entry is always the on-list item holding the run's last unit.
func TestSplitItem_KeyTracksRightHalf(t *testing.T) {
	base, _ := hex.DecodeString(splitEntryBase)
	doc := newTestDoc(5)
	if err := ApplyUpdateV1(doc, base, nil); err != nil {
		t.Fatal(err)
	}
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		right := doc.store.getItemCleanStart(txn, ID{Client: 10, Clock: 1})
		if got := m.itemMap["k"]; got != right {
			t.Fatalf("after split itemMap[k]=%v, want the right half %v", got.ID, right.ID)
		}
	})
	got := m.itemMap["k"]
	if items := doc.store.clients[10]; len(items) != 1 || items[0] != got {
		t.Fatalf("after re-merge itemMap[k]=%v is not the store's single run", got.ID)
	}
	if got.lastID() != (ID{Client: 10, Clock: 2}) {
		t.Fatalf("itemMap[k] last id %v, want 10:2", got.lastID())
	}
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", float64(99)) })
	if v, ok := m.Get("k"); !ok || v != float64(99) {
		t.Fatalf("k = %v, %v; want 99", v, ok)
	}
	fresh := New()
	if err := ApplyUpdateV1(fresh, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	if v, ok := fresh.GetMap("m").Get("k"); !ok || fmt.Sprint(v) != "99" {
		t.Fatalf("fresh peer k = %v, %v; want 99", v, ok)
	}
}
