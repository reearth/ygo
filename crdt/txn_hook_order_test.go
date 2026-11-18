package crdt

import (
	"slices"
	"testing"
)

// A committed transaction calls its hooks in Yjs's cleanupTransactions order:
// type observers, deep observers, afterTransaction, update, subdocs.
func TestUnit_Transact_HookOrderMatchesYjs(t *testing.T) {
	change := func(txn *Transaction) {
		m := txn.GetMap("m")
		m.Set(txn, "k", 1)
		m.Set(txn, "sub", New(WithGUID("sub")))
	}
	src := newTestDoc(1)
	src.Transact(change)
	update := EncodeStateAsUpdateV1(src, nil)

	for _, tc := range []struct {
		name  string
		apply func(d *Doc) error
	}{
		{"local", func(d *Doc) error { d.Transact(change); return nil }},
		{"remote", func(d *Doc) error { return ApplyUpdateV1(d, update, "remote") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDoc(2)
			var got []string
			m := d.GetMap("m")
			m.Observe(func(YMapEvent) { got = append(got, "observe") })
			m.ObserveDeep(func(*Transaction) { got = append(got, "observeDeep") })
			d.OnAfterTransaction(func(*Transaction) { got = append(got, "afterTransaction") })
			d.OnUpdate(func([]byte, any) { got = append(got, "update") })
			d.OnSubdocs(func(SubdocsEvent) { got = append(got, "subdocs") })
			if err := tc.apply(d); err != nil {
				t.Fatal(err)
			}
			want := []string{"observe", "observeDeep", "afterTransaction", "update", "subdocs"}
			if !slices.Equal(got, want) {
				t.Fatalf("hook order\n got %v\nwant %v", got, want)
			}
		})
	}
}
