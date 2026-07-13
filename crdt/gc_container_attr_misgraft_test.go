package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGC_DeletedContainerAttr_NoMapGraft is a convergence regression for #156.
//
// When an XML element is deleted, auto-GC replaces its ContentType with a
// ContentDeleted placeholder, so a concurrent attribute write on that element
// arrives with its parent lost from the wire (a keyed item whose origin chain
// no longer resolves to a live container). Such an orphan must be dropped on
// EVERY peer, matching Yjs, rather than mis-grafted onto an arbitrary root map
// via a store-wide "find any map parent" fallback. Before the fix the orphaned
// `id` attribute landed on the root map `m` as a spurious key on whichever peer
// happened to have a live map entry integrated at resolve time, so two peers
// that saw the same updates in different orders diverged.
//
// Found by the #70 convergence fuzzer (seeds 123, 267, 286) and reproduced here
// harness-free over both the V1 and V2 wire paths.
func TestGC_DeletedContainerAttr_NoMapGraft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encode func(*Doc) []byte
		apply  func(*Doc, []byte) error
	}{
		{"V1",
			func(d *Doc) []byte { return EncodeStateAsUpdateV1(d, nil) },
			func(d *Doc, u []byte) error { return ApplyUpdateV1(d, u, nil) }},
		{"V2",
			func(d *Doc) []byte { return EncodeStateAsUpdateV2(d, nil) },
			func(d *Doc, u []byte) error { return ApplyUpdateV2(d, u, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Peer A: root map m with one key, plus a <div> in xml fragment x.
			a := New(WithClientID(1))
			am, ax := a.GetMap("m"), a.GetXmlFragment("x")
			a.Transact(func(txn *Transaction) {
				am.Set(txn, "k0", 1)
				ax.InsertElement(txn, 0, NewYXmlElement("div"))
			})

			// Peer B receives the div, then sets its `id` attribute twice. The
			// second write is LWW so its origin is the first write and it carries
			// no on-wire parent — it inherits the key from its origin.
			b := New(WithClientID(2))
			require.NoError(t, tc.apply(b, tc.encode(a)))
			bx := b.GetXmlFragment("x")
			b.Transact(func(txn *Transaction) {
				el := bx.Children()[0].(*YXmlElement)
				el.SetAttribute(txn, "id", "Y")
				el.SetAttribute(txn, "id", "Z")
			})

			// Peer A concurrently deletes the div; a follow-up empty transaction
			// forces auto-GC of the tombstone (ContentType -> ContentDeleted).
			a.Transact(func(txn *Transaction) { ax.Delete(txn, 0, 1) })
			a.Transact(func(*Transaction) {})

			uA := tc.encode(a) // create div + delete + map k0
			uB := tc.encode(b) // id="Y" then id="Z" on the div

			// Two fresh peers integrate the identical updates in opposite orders.
			p := New(WithClientID(10)) // id-writes while div still live, then deletion
			require.NoError(t, tc.apply(p, uB))
			require.NoError(t, tc.apply(p, uA))

			q := New(WithClientID(11)) // deletion first, then id-writes (div gone)
			require.NoError(t, tc.apply(q, uA))
			require.NoError(t, tc.apply(q, uB))

			pm, err := p.GetMap("m").ToJSON()
			require.NoError(t, err)
			qm, err := q.GetMap("m").ToJSON()
			require.NoError(t, err)

			// Both peers must converge, and neither may carry the orphaned `id`
			// attribute as a root-map key.
			require.Equal(t, string(pm), string(qm), "peers diverged on root map m")
			require.NotContains(t, string(pm), `"id"`,
				"orphaned xml attribute mis-grafted onto root map")
			require.NotContains(t, string(qm), `"id"`,
				"orphaned xml attribute mis-grafted onto root map")
		})
	}
}
