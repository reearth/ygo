package crdt

import "testing"

// Staging a detached type into itself, or into a type staged (transitively)
// inside it, builds a cyclic staged tree: every recursive read (ToJSON, ToXML)
// overflows the stack and the attach never terminates. Staging must reject the
// cycle at the call site, as it rejects double-staging (#222).

func TestUnit_StageCycle_MapAndArray_PanicsAtCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wantFn string
		build  func(txn *Transaction) (cycle func(), read func() any)
	}{
		{"map self", "Set", func(txn *Transaction) (func(), func() any) {
			p := NewMapPrelim()
			return func() { p.Set(txn, "self", p) }, func() any { b, _ := p.ToJSON(); return b }
		}},
		{"array self PushType", "PushType", func(txn *Transaction) (func(), func() any) {
			a := NewArrayPrelim()
			return func() { a.PushType(txn, a) }, func() any { b, _ := a.ToJSON(); return b }
		}},
		{"array self InsertType", "InsertType", func(txn *Transaction) (func(), func() any) {
			a := NewArrayPrelim()
			return func() { a.InsertType(txn, 0, a) }, func() any { b, _ := a.ToJSON(); return b }
		}},
		{"map into its own child map", "Set", func(txn *Transaction) (func(), func() any) {
			p, q := NewMapPrelim(), NewMapPrelim()
			q.Set(txn, "x", p)
			return func() { p.Set(txn, "y", q) }, func() any { b, _ := p.ToJSON(); return b }
		}},
		{"array into its own child array", "PushType", func(txn *Transaction) (func(), func() any) {
			a, b := NewArrayPrelim(), NewArrayPrelim()
			b.PushType(txn, a)
			return func() { a.PushType(txn, b) }, func() any { b, _ := a.ToJSON(); return b }
		}},
		{"map into its grandchild array", "InsertType", func(txn *Transaction) (func(), func() any) {
			top, mid, leaf := NewMapPrelim(), NewArrayPrelim(), NewArrayPrelim()
			top.Set(txn, "mid", mid)
			mid.PushType(txn, leaf)
			return func() { leaf.InsertType(txn, 0, top) }, func() any { b, _ := top.ToJSON(); return b }
		}},
		{"array into its grandchild map", "Set", func(txn *Transaction) (func(), func() any) {
			top, mid, leaf := NewArrayPrelim(), NewMapPrelim(), NewMapPrelim()
			top.PushType(txn, mid)
			mid.Set(txn, "leaf", leaf)
			return func() { leaf.Set(txn, "top", top) }, func() any { b, _ := top.ToJSON(); return b }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			doc.Transact(func(txn *Transaction) {
				cycle, _ := tc.build(txn)
				mustPanicContaining(t, tc.wantFn+": staging this type here would create a cycle", cycle)
			})
			// The rejected call must leave the staged tree acyclic and readable.
			doc.Transact(func(txn *Transaction) {
				cycle, read := tc.build(txn)
				func() {
					defer func() { _ = recover() }()
					cycle()
				}()
				_ = read()
			})
		})
	}
}

// A handle that LEFT its container (overwrite, delete) no longer sits inside
// it, so staging the old container into it is not a cycle.
func TestUnit_StageCycle_DisplacedHandleMayHostItsOldContainer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		displace func(txn *Transaction, outer *YArray, inner *YMap)
	}{
		{"array delete", func(txn *Transaction, outer *YArray, inner *YMap) {
			outer.PushType(txn, inner)
			outer.Delete(txn, 0, 1)
		}},
		{"map overwrite", func(txn *Transaction, outer *YArray, inner *YMap) {
			m := NewMapPrelim()
			m.Set(txn, "k", inner)
			outer.PushType(txn, m)
			m.Set(txn, "k", "plain")
			outer.Delete(txn, 0, 1)
		}},
		{"map delete", func(txn *Transaction, outer *YArray, inner *YMap) {
			m := NewMapPrelim()
			m.Set(txn, "k", inner)
			outer.PushType(txn, m)
			m.Delete(txn, "k")
			outer.Delete(txn, 0, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			root := doc.GetMap("root")
			outer, inner := NewArrayPrelim(), NewMapPrelim()
			doc.Transact(func(txn *Transaction) {
				tc.displace(txn, outer, inner)
				outer.Push(txn, []any{"v"})
				inner.Set(txn, "outer", outer)
				root.Set(txn, "inner", inner)
			})
			got, err := root.ToJSON()
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"inner":{"outer":["v"]}}`; string(got) != want {
				t.Fatalf("root = %s, want %s", got, want)
			}
		})
	}
}

func TestUnit_StageCycle_XML_PanicsAtCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(txn *Transaction) (cycle func(), read func() string)
	}{
		{"self", func(txn *Transaction) (func(), func() string) {
			a := NewYXmlElement("a")
			return func() { a.InsertElement(txn, 0, a) }, a.ToXML
		}},
		{"into its child", func(txn *Transaction) (func(), func() string) {
			a, b := NewYXmlElement("a"), NewYXmlElement("b")
			a.InsertElement(txn, 0, b)
			return func() { b.InsertElement(txn, 0, a) }, a.ToXML
		}},
		{"into its grandchild, among siblings", func(txn *Transaction) (func(), func() string) {
			a, b, c := NewYXmlElement("a"), NewYXmlElement("b"), NewYXmlElement("c")
			a.Insert(txn, 0, NewYXmlText(), b)
			b.InsertElement(txn, 0, c)
			return func() { c.Insert(txn, 0, NewYXmlElement("ok"), a) }, a.ToXML
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			doc.Transact(func(txn *Transaction) {
				cycle, _ := tc.build(txn)
				mustPanicContaining(t, "Insert: staging this node here would create a cycle", cycle)
			})
			doc.Transact(func(txn *Transaction) {
				cycle, read := tc.build(txn)
				func() {
					defer func() { _ = recover() }()
					cycle()
				}()
				_ = read()
			})
		})
	}
}

// A detached XML node removed from its parent may host that parent.
func TestUnit_StageCycle_XML_DeletedChildMayHostItsOldParent(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("f")
	a, b := NewYXmlElement("a"), NewYXmlElement("b")
	doc.Transact(func(txn *Transaction) {
		a.InsertElement(txn, 0, b)
		a.Delete(txn, 0, 1)
		b.InsertElement(txn, 0, a)
		frag.InsertElement(txn, 0, b)
	})
	if got, want := frag.ToXML(), "<b><a></a></b>"; got != want {
		t.Fatalf("ToXML = %q, want %q", got, want)
	}
}

// A detached fragment staged inside an element must not hide a cycle.
func TestUnit_StageCycle_XML_ThroughNestedFragment(t *testing.T) {
	newFrag := func() *YXmlFragment {
		f := &YXmlFragment{}
		f.itemMap = make(map[string]*Item)
		f.owner = f
		return f
	}
	doc := newTestDoc(1)
	doc.Transact(func(txn *Transaction) {
		a, f := NewYXmlElement("a"), newFrag()
		a.Insert(txn, 0, f)
		mustPanicContaining(t, "Insert: staging this node here would create a cycle",
			func() { f.Insert(txn, 0, a) })
		if f.Len() != 0 {
			t.Fatalf("rejected insert was buffered: Len=%d", f.Len())
		}

		outer, inner := newFrag(), newFrag()
		outer.Insert(txn, 0, inner)
		mustPanicContaining(t, "Insert: staging this node here would create a cycle",
			func() { inner.Insert(txn, 0, outer) })
	})
}
