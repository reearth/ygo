package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A format value can be a map: a link mark's attributes, as y-prosemirror and
// Quill store them. Observing such a text used to panic while the observer
// delta was built ("comparing uncomparable type map[string]interface {}"),
// under the document lock, which then stayed held.
func TestUnit_YText_Observe_MapValuedFormat(t *testing.T) {
	link := func(href string) Attributes { return Attributes{"link": map[string]any{"href": href}} }

	for _, tc := range []struct {
		name string
		edit func(txn *Transaction, txt *YText)
		want []Delta
	}{
		{
			name: "insert inside the formatted run",
			edit: func(txn *Transaction, txt *YText) { txt.Insert(txn, 5, "!", nil) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 5},
				{Op: DeltaOpInsert, Insert: "!", Attributes: link("https://a.example")},
			},
		},
		{
			name: "delete inside the formatted run",
			edit: func(txn *Transaction, txt *YText) { txt.Delete(txn, 1, 2) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 1},
				{Op: DeltaOpDelete, Delete: 2},
			},
		},
		{
			name: "format with another map value",
			edit: func(txn *Transaction, txt *YText) { txt.Format(txn, 0, 5, link("https://b.example")) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 5, Attributes: link("https://b.example")},
			},
		},
	} {
		for _, remote := range []bool{false, true} {
			name := tc.name + "/local"
			if remote {
				name = tc.name + "/remote"
			}
			t.Run(name, func(t *testing.T) {
				writer := newTestDoc(1)
				wtxt := writer.GetText("t")
				writer.Transact(func(txn *Transaction) {
					wtxt.Insert(txn, 0, "hello", link("https://a.example"))
				})

				doc, txt := writer, wtxt
				var update []byte
				if remote {
					doc = newTestDoc(2)
					txt = doc.GetText("t")
					require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(writer, nil), nil))
					writer.OnUpdate(func(u []byte, _ any) { update = u })
				}
				var got []Delta
				txt.Observe(func(e YTextEvent) { got = e.Delta })

				var panicked any
				func() {
					defer func() { panicked = recover() }()
					writer.Transact(func(txn *Transaction) { tc.edit(txn, wtxt) })
					if remote {
						require.NoError(t, ApplyUpdateV1(doc, update, nil))
					}
				}()
				require.Nil(t, panicked, "the edit panicked")
				assert.Equal(t, tc.want, got)
				assert.Equal(t, wtxt.ToDelta(), txt.ToDelta())
			})
		}
	}
}

// panicOnFire is a shared type whose observer events cannot be built.
type panicOnFire struct{ abstractType }

func (p *panicOnFire) baseType() *abstractType { return &p.abstractType }
func (p *panicOnFire) prepareFire(*Transaction, map[string]struct{}) func() {
	panic("prepareFire failed")
}

// A panic while a committed transaction's observer events are built, under
// the document lock, reaches the caller with the lock released, as a panic in
// the Transact callback does.
func TestUnit_Transact_PanicBuildingObserverEventsReleasesLock(t *testing.T) {
	doc := newTestDoc(1)
	bad := &panicOnFire{}
	bad.doc = doc
	bad.owner = bad
	m := doc.GetMap("m")

	var panicked any
	func() {
		defer func() { panicked = recover() }()
		doc.Transact(func(txn *Transaction) {
			m.Set(txn, "k", "v")
			txn.addChanged(&bad.abstractType, "")
		})
	}()
	require.Equal(t, "prepareFire failed", panicked)

	locked := !doc.mu.TryLock()
	if !locked {
		doc.mu.Unlock()
	}
	require.False(t, locked, "the document lock is still held after the panic")
	v, ok := m.Get("k")
	assert.True(t, ok)
	assert.Equal(t, "v", v)
}
