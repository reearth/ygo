package crdt

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// observerChain registers two type observers and two deep observers on the
// text "t", two OnUpdate, two OnAfterTransaction and two OnSubdocs observers on
// doc, in that order. Each appends its name to calls and, while armed, panics
// with its name if panics lists it.
type observerChain struct {
	calls  []string
	update []byte
	armed  bool
}

func newObserverChain(doc *Doc, panics ...string) *observerChain {
	c := &observerChain{armed: true}
	call := func(name string) {
		c.calls = append(c.calls, name)
		if c.armed && slices.Contains(panics, name) {
			panic(name)
		}
	}
	txt := doc.GetText("t")
	txt.Observe(func(YTextEvent) { call("type1") })
	txt.Observe(func(YTextEvent) { call("type2") })
	txt.ObserveDeep(func(*Transaction) { call("deep1") })
	txt.ObserveDeep(func(*Transaction) { call("deep2") })
	doc.OnUpdate(func(u []byte, _ any) { c.update = u; call("update1") })
	doc.OnUpdate(func([]byte, any) { call("update2") })
	doc.OnAfterTransaction(func(*Transaction) { call("after1") })
	doc.OnAfterTransaction(func(*Transaction) { call("after2") })
	doc.OnSubdocs(func(SubdocsEvent) { call("subdocs1") })
	doc.OnSubdocs(func(SubdocsEvent) { call("subdocs2") })
	return c
}

var allObservers = []string{"type1", "type2", "deep1", "deep2", "update1", "update2", "after1", "after2", "subdocs1", "subdocs2"}

// writeTextAndSubdoc inserts "a" into the text "t" and a subdocument into the
// map "m", so every observer of an observerChain has something to report.
func writeTextAndSubdoc(txn *Transaction, guid string) {
	txn.GetText("t").Insert(txn, 0, "a", nil)
	txn.GetMap("m").Set(txn, "sub", New(WithGUID(guid)))
}

// recoverPanic runs fn and returns the value it panicked with, or nil.
func recoverPanic(fn func()) (p any) {
	defer func() { p = recover() }()
	fn()
	return nil
}

// TestUnit_Doc_PanickingObserver_RunsTheRest checks that, as in Yjs, a
// panicking observer does not stop the others: every observer runs, OnUpdate
// gets the transaction's update, the last panic reaches the caller, and the
// document still takes the next transaction.
func TestUnit_Doc_PanickingObserver_RunsTheRest(t *testing.T) {
	tests := []struct {
		name     string
		panics   []string
		fnPanics bool
		remote   bool
		want     any
	}{
		{name: "first type observer", panics: []string{"type1"}, want: "type1"},
		{name: "second type observer", panics: []string{"type2"}, want: "type2"},
		{name: "first deep observer", panics: []string{"deep1"}, want: "deep1"},
		{name: "first OnUpdate observer", panics: []string{"update1"}, want: "update1"},
		{name: "first OnAfterTransaction observer", panics: []string{"after1"}, want: "after1"},
		{name: "first OnSubdocs observer", panics: []string{"subdocs1"}, want: "subdocs1"},
		{name: "two observers, the later panic reaches the caller", panics: []string{"type1", "update2"}, want: "update2"},
		{name: "every observer", panics: allObservers, want: "subdocs2"},
		{name: "transaction fn and an observer", panics: []string{"deep2"}, fnPanics: true, want: "deep2"},
		{name: "remote apply, first type observer", panics: []string{"type1"}, remote: true, want: "type1"},
		{name: "remote apply, first OnUpdate observer", panics: []string{"update1"}, remote: true, want: "update1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := newTestDoc(1)
			chain := newObserverChain(doc, tt.panics...)

			assert.PanicsWithValue(t, tt.want, func() {
				if tt.remote {
					src := newTestDoc(2)
					src.Transact(func(txn *Transaction) { writeTextAndSubdoc(txn, "sub") })
					_ = ApplyUpdateV1(doc, src.EncodeStateAsUpdate(), nil)
					return
				}
				doc.Transact(func(txn *Transaction) {
					writeTextAndSubdoc(txn, "sub")
					if tt.fnPanics {
						panic("fn")
					}
				})
			})

			assert.Equal(t, allObservers, chain.calls, "observers called")
			replica := newTestDoc(3)
			require.NoError(t, ApplyUpdateV1(replica, chain.update, nil), "apply the OnUpdate update")
			assert.Equal(t, "a", replica.GetText("t").ToString(), "text from the OnUpdate update")

			chain.armed, chain.calls = false, nil
			transactWithin(t, doc, func(txn *Transaction) { writeTextAndSubdoc(txn, "sub2") })
			assert.Equal(t, allObservers, chain.calls, "observers called by the next transaction")
		})
	}
}

// TestUnit_Doc_PanickingObserver_EveryTypeRunsTheRest checks, for each shared
// type, that its second observer runs when the first one panics.
func TestUnit_Doc_PanickingObserver_EveryTypeRunsTheRest(t *testing.T) {
	tests := []struct {
		name string
		// observe registers obs twice on a type of doc and returns a write to it.
		observe func(doc *Doc, obs func()) func(*Transaction)
	}{
		{"YMap", func(doc *Doc, obs func()) func(*Transaction) {
			m := doc.GetMap("m")
			m.Observe(func(YMapEvent) { obs() })
			m.Observe(func(YMapEvent) { obs() })
			return func(txn *Transaction) { m.Set(txn, "k", 1) }
		}},
		{"YArray", func(doc *Doc, obs func()) func(*Transaction) {
			a := doc.GetArray("a")
			a.Observe(func(YArrayEvent) { obs() })
			a.Observe(func(YArrayEvent) { obs() })
			return func(txn *Transaction) { a.Push(txn, []any{1}) }
		}},
		{"YText", func(doc *Doc, obs func()) func(*Transaction) {
			txt := doc.GetText("t")
			txt.Observe(func(YTextEvent) { obs() })
			txt.Observe(func(YTextEvent) { obs() })
			return func(txn *Transaction) { txt.Insert(txn, 0, "a", nil) }
		}},
		{"YXmlFragment", func(doc *Doc, obs func()) func(*Transaction) {
			f := doc.GetXmlFragment("x")
			f.Observe(func(YXmlEvent) { obs() })
			f.Observe(func(YXmlEvent) { obs() })
			return func(txn *Transaction) { f.InsertElement(txn, 0, NewYXmlElement("p")) }
		}},
		{"YXmlElement", func(doc *Doc, obs func()) func(*Transaction) {
			e := NewYXmlElement("p")
			f := doc.GetXmlFragment("x")
			doc.Transact(func(txn *Transaction) { f.InsertElement(txn, 0, e) })
			e.Observe(func(YXmlEvent) { obs() })
			e.Observe(func(YXmlEvent) { obs() })
			return func(txn *Transaction) { e.SetAttribute(txn, "k", "v") }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := newTestDoc(1)
			calls := 0
			write := tt.observe(doc, func() {
				calls++
				if calls == 1 {
					panic("first")
				}
			})
			assert.PanicsWithValue(t, "first", func() { doc.Transact(write) })
			assert.Equal(t, 2, calls, "observers called")
		})
	}
}

// TestUnit_Doc_PanickingObserver_OtherTypesRun checks that a panicking
// observer of one type does not stop the observers of another type, and that
// OnSubdocs still runs only when a subdocument changed.
func TestUnit_Doc_PanickingObserver_OtherTypesRun(t *testing.T) {
	doc := newTestDoc(1)
	a, b := doc.GetMap("a"), doc.GetMap("b")
	var calls []string
	a.Observe(func(YMapEvent) { calls = append(calls, "a"); panic("a") })
	b.Observe(func(YMapEvent) { calls = append(calls, "b"); panic("b") })
	doc.OnSubdocs(func(SubdocsEvent) { calls = append(calls, "subdocs") })

	got := recoverPanic(func() {
		doc.Transact(func(txn *Transaction) { a.Set(txn, "k", 1); b.Set(txn, "k", 1) })
	})

	require.ElementsMatch(t, []string{"a", "b"}, calls, "observers called")
	assert.Equal(t, calls[1], got, "the later panic reaches the caller")
}

// TestUnit_Doc_PanickingObserver_ParentDeepObserversRun checks that a
// panicking deep observer of a nested map does not stop the deep observers
// of its parent.
func TestUnit_Doc_PanickingObserver_ParentDeepObserversRun(t *testing.T) {
	doc := newTestDoc(1)
	root := doc.GetMap("root")
	doc.Transact(func(txn *Transaction) { root.Set(txn, "child", NewMapPrelim()) })
	v, _ := root.Get("child")
	child := v.(*YMap)
	var calls []string
	child.ObserveDeep(func(*Transaction) { calls = append(calls, "child"); panic("child") })
	root.ObserveDeep(func(*Transaction) { calls = append(calls, "root") })

	assert.PanicsWithValue(t, "child", func() {
		doc.Transact(func(txn *Transaction) { child.Set(txn, "k", 1) })
	})
	assert.Equal(t, []string{"child", "root"}, calls, "deep observers called")
}

// TestUnit_Doc_PanickingObserver_FormatCleanupRunsTheRest checks that the
// after-transaction observers of a format cleanup that deletes nothing all run
// when the first one panics.
func TestUnit_Doc_PanickingObserver_FormatCleanupRunsTheRest(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	a.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "abc", Attributes{"bold": true}) })
	b.GetText("t") // a root placeholder gets no format cleanup
	var calls []string
	b.OnAfterTransaction(func(txn *Transaction) {
		calls = append(calls, fmt.Sprintf("after1 local=%t", txn.Local))
		if txn.Local {
			panic("after1")
		}
	})
	b.OnAfterTransaction(func(txn *Transaction) { calls = append(calls, fmt.Sprintf("after2 local=%t", txn.Local)) })

	assert.PanicsWithValue(t, "after1", func() { _ = ApplyUpdateV1(b, a.EncodeStateAsUpdate(), nil) })
	assert.Equal(t, []string{"after1 local=false", "after2 local=false", "after1 local=true", "after2 local=true"}, calls)
}

// TestUnit_Doc_PanickingObserver_SkipsFormatCleanupDecision checks that a
// text observer of a remote change that gives the text its first format marker
// starts a format cleanup only when it does not panic, as in Yjs, where a
// throwing observer skips YText._callObserver's _needFormattingCleanup.
func TestUnit_Doc_PanickingObserver_SkipsFormatCleanupDecision(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(fmt.Sprintf("panics=%t", panics), func(t *testing.T) {
			a, b := newTestDoc(1), newTestDoc(2)
			at, bt := a.GetText("t"), b.GetText("t")
			a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
			require.NoError(t, ApplyUpdateV1(b, a.EncodeStateAsUpdate(), nil))
			a.Transact(func(txn *Transaction) { at.Insert(txn, 3, "x", nil) })
			var origins []any
			b.OnAfterTransaction(func(txn *Transaction) { origins = append(origins, txn.Origin) })
			reacted := false
			bt.Observe(func(e YTextEvent) {
				if e.Txn.Local || reacted {
					return
				}
				reacted = true
				b.Transact(func(txn *Transaction) { bt.Format(txn, 0, 1, Attributes{"bold": true}) }, "react")
				if panics {
					panic("observer")
				}
			})

			got := recoverPanic(func() {
				_ = ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, b.StateVector()), "remote")
			})

			want := []any{"react", "remote", nil} // nil: the cleanup
			if panics {
				assert.Equal(t, "observer", got)
				want = want[:2]
			} else {
				assert.Nil(t, got)
			}
			assert.Equal(t, want, origins, "after-transaction origins")
		})
	}
}

// TestUnit_Doc_PanickingObserver_LaterObserverTransacts checks that an
// observer running after another one panicked can open its own transaction,
// whose observers run, without that transaction recovering the panic.
func TestUnit_Doc_PanickingObserver_LaterObserverTransacts(t *testing.T) {
	doc := newTestDoc(1)
	m, n := doc.GetMap("m"), doc.GetMap("n")
	var calls []string
	m.Observe(func(YMapEvent) { calls = append(calls, "m1"); panic("m1") })
	m.Observe(func(YMapEvent) {
		calls = append(calls, "m2")
		doc.Transact(func(txn *Transaction) { n.Set(txn, "x", 1) })
	})
	n.Observe(func(YMapEvent) { calls = append(calls, "n") })

	assert.PanicsWithValue(t, "m1", func() {
		doc.Transact(func(txn *Transaction) { m.Set(txn, "a", 1) })
	})
	assert.Equal(t, []string{"m1", "m2", "n"}, calls, "observers called")
	v, _ := n.Get("x")
	assert.Equal(t, int64(1), v, "n.x")
}

// transactWithin runs fn in a transaction on doc and fails the test if the
// transaction does not finish, which is how a held document lock shows.
func transactWithin(t *testing.T, doc *Doc, fn func(*Transaction)) {
	t.Helper()
	done := make(chan any, 1)
	go func() { done <- recoverPanic(func() { doc.Transact(fn) }) }()
	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("transaction panicked: %v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("transaction did not finish: the document lock is held")
	}
}
