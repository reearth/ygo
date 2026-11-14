package crdt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A format value can be a map or a slice: a link mark's attributes, as
// y-prosemirror and Quill store them. Observing such a text used to panic
// while the observer delta was built, under the document lock.
func TestUnit_YText_Observe_MapValuedFormat(t *testing.T) {
	link := func(href string) Attributes { return Attributes{"link": map[string]any{"href": href}} }
	comments := Attributes{"comments": []any{"c1", "c2"}}

	for _, tc := range []struct {
		name   string
		xml    bool // the text is a YXmlText in the root fragment "x"
		format Attributes
		edit   func(txn *Transaction, txt *YText)
		want   []Delta
	}{
		{
			name:   "insert inside the formatted run",
			format: link("https://a.example"),
			edit:   func(txn *Transaction, txt *YText) { txt.Insert(txn, 5, "!", nil) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 5},
				{Op: DeltaOpInsert, Insert: "!", Attributes: link("https://a.example")},
			},
		},
		{
			name:   "delete inside the formatted run",
			format: link("https://a.example"),
			edit:   func(txn *Transaction, txt *YText) { txt.Delete(txn, 1, 2) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 1},
				{Op: DeltaOpDelete, Delete: 2},
			},
		},
		{
			name:   "format with another map value",
			format: link("https://a.example"),
			edit:   func(txn *Transaction, txt *YText) { txt.Format(txn, 0, 5, link("https://b.example")) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 5, Attributes: link("https://b.example")},
			},
		},
		{
			name:   "replace the map value with nil in part of the run",
			format: link("https://a.example"),
			edit:   func(txn *Transaction, txt *YText) { txt.Format(txn, 1, 2, Attributes{"link": nil}) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 1},
				{Op: DeltaOpRetain, Retain: 2, Attributes: Attributes{"link": nil}},
			},
		},
		{
			name:   "insert inside a slice-valued run",
			format: comments,
			edit:   func(txn *Transaction, txt *YText) { txt.Insert(txn, 5, "!", nil) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 5},
				{Op: DeltaOpInsert, Insert: "!", Attributes: comments},
			},
		},
		{
			name:   "insert inside the formatted run of a YXmlText",
			xml:    true,
			format: link("https://a.example"),
			edit:   func(txn *Transaction, txt *YText) { txt.Insert(txn, 5, "!", nil) },
			want: []Delta{
				{Op: DeltaOpRetain, Retain: 5},
				{Op: DeltaOpInsert, Insert: "!", Attributes: link("https://a.example")},
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
				var wtxt *YText
				writer.Transact(func(txn *Transaction) {
					if tc.xml {
						x := NewYXmlText()
						txn.GetXmlFragment("x").InsertText(txn, 0, x)
						wtxt = &x.YText
					} else {
						wtxt = txn.GetText("t")
					}
					wtxt.Insert(txn, 0, "hello", tc.format)
				})

				doc, txt := writer, wtxt
				var update []byte
				if remote {
					doc = newTestDoc(2)
					require.NoError(t, ApplyUpdateV1(doc, EncodeStateAsUpdateV1(writer, nil), nil))
					txt = doc.GetText("t")
					if tc.xml {
						txt = &doc.GetXmlFragment("x").Children()[0].(*YXmlText).YText
					}
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

func newPanicOnFire(doc *Doc) *panicOnFire {
	p := &panicOnFire{}
	p.doc = doc
	p.owner = p
	return p
}

func (p *panicOnFire) baseType() *abstractType { return &p.abstractType }
func (p *panicOnFire) prepareFire(*Transaction, map[string]struct{}) func() {
	panic("prepareFire failed")
}

// transactPanic runs fn in a transaction on doc and returns the value the
// transaction panicked with, or nil.
func transactPanic(doc *Doc, fn func(*Transaction)) (panicked any) {
	defer func() { panicked = recover() }()
	doc.Transact(fn)
	return nil
}

// A panic while a committed transaction's observer events are built, under
// the document lock, reaches the caller with the lock released, as a panic in
// the Transact callback does.
func TestUnit_Transact_PanicBuildingObserverEventsReleasesLock(t *testing.T) {
	doc := newTestDoc(1)
	bad := newPanicOnFire(doc)
	m := doc.GetMap("m")

	panicked := transactPanic(doc, func(txn *Transaction) {
		m.Set(txn, "k", "v")
		txn.addChanged(&bad.abstractType, "")
	})
	require.Equal(t, "prepareFire failed", panicked)

	locked := !doc.mu.TryLock()
	if !locked {
		doc.mu.Unlock()
	}
	assert.False(t, locked, "the document lock is still held after the panic")

	done := make(chan struct{})
	go func() {
		doc.Transact(func(txn *Transaction) { m.Set(txn, "k2", "w") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second Transact still waits for the lock after the panic")
	}
	v, ok := m.Get("k")
	assert.True(t, ok)
	assert.Equal(t, "v", v)
	v, ok = m.Get("k2")
	assert.True(t, ok)
	assert.Equal(t, "w", v)
}

// A type whose observer event cannot be built skips only its own observers:
// the rest of the transaction's observers fire before the panic reaches the
// caller, so a committed edit is still announced.
func TestUnit_Transact_PanicBuildingOneTypesEventFiresTheOtherObservers(t *testing.T) {
	doc := newTestDoc(1)
	bad := newPanicOnFire(doc)
	m := doc.GetMap("m")

	var mapEvents, deepEvents, afterTxns int
	var update []byte
	var subdocs SubdocsEvent
	m.Observe(func(YMapEvent) { mapEvents++ })
	m.ObserveDeep(func(*Transaction) { deepEvents++ })
	doc.OnUpdate(func(u []byte, _ any) { update = u })
	doc.OnAfterTransaction(func(*Transaction) { afterTxns++ })
	doc.OnSubdocs(func(e SubdocsEvent) { subdocs = e })

	panicked := transactPanic(doc, func(txn *Transaction) {
		m.Set(txn, "k", "v")
		m.Set(txn, "sub", New(WithGUID("child")))
		txn.addChanged(&bad.abstractType, "")
	})
	require.Equal(t, "prepareFire failed", panicked)

	assert.Equal(t, 1, mapEvents, "the map's observer")
	assert.Equal(t, 1, deepEvents, "the map's deep observer")
	assert.Equal(t, 1, afterTxns, "OnAfterTransaction")
	assert.Len(t, subdocs.Added, 1, "OnSubdocs")
	assert.Equal(t, []string{"child"}, doc.GetSubdocGUIDs(), "the subdoc registry")
	require.NotEmpty(t, update, "OnUpdate")
	replica := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(replica, update, nil))
	v, _ := replica.GetMap("m").Get("k")
	assert.Equal(t, "v", v, "the update OnUpdate received")
}
