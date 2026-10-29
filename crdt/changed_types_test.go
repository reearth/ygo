package crdt

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// The contract these tests hold Transaction.ChangedTypes and Parent to is the
// one an owner that re-renders only the blocks a remote update touched relies
// on: after a remote apply, every top-level child of an XML fragment whose
// content changed is reached by walking Parent from some ChangedTypes entry,
// unless the fragment itself is an entry (its children were inserted, deleted
// or reordered). A writer document builds a ProseMirror-like tree and a
// receiver applies each of the writer's transactions remotely.

// ctMaxWalk bounds a Parent walk so a cycle fails the test instead of hanging.
const ctMaxWalk = 64

// ctHarness is one writer/receiver pair driven by a seeded generator.
type ctHarness struct {
	t   *testing.T
	rng *rand.Rand

	w     *Doc
	wFrag *YXmlFragment
	wMeta *YMap
	wSub  *YMap

	r     *Doc
	rFrag *YXmlFragment
	rMeta *YMap
	rSub  *YMap

	update   []byte       // the writer's last update, from OnUpdate
	wChanged int          // how many types the writer's last transaction changed
	changed  []SharedType // ChangedTypes of the receiver's remote transactions
	touched  ctRoots      // which writer roots the current step changed
	stats    ctStats      // how often each branch of the check ran
	words    []string     // text runs the generator inserts
	attrs    []Attributes // formatting the generator inserts and applies
}

type ctRoots uint8

const (
	ctTouchFrag ctRoots = 1 << iota
	ctTouchMeta
)

type ctStats struct {
	fragInSet      int // steps whose ChangedTypes named the fragment
	blocksAttested int // changed blocks found by walking Parent
	metaOnly       int // steps that changed only the other root
}

func newCTHarness(t *testing.T, seed uint64) *ctHarness {
	t.Helper()
	h := &ctHarness{
		t:     t,
		rng:   rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		w:     New(WithClientID(1)),
		r:     New(WithClientID(2)),
		words: []string{"a", "bc", "def", "ghij", " ", "<&>", "é", "xyz"},
		attrs: []Attributes{
			nil, nil,
			{"bold": true},
			{"italic": true},
			{"link": map[string]any{"href": "https://example.com"}},
			{"bold": nil},
			{"italic": nil},
		},
	}
	h.wFrag = h.w.GetXmlFragment("prosemirror")
	h.wMeta = h.w.GetMap("meta")
	h.rFrag = h.r.GetXmlFragment("prosemirror")
	h.rMeta = h.r.GetMap("meta")

	h.w.OnUpdate(func(update []byte, _ any) {
		h.update = bytes.Clone(update)
	})
	h.w.OnAfterTransaction(func(txn *Transaction) {
		h.wChanged = len(txn.ChangedTypes())
	})
	h.r.OnAfterTransaction(func(txn *Transaction) {
		if txn.Local {
			return
		}
		h.changed = append(h.changed, txn.ChangedTypes()...)
	})

	h.w.Transact(func(txn *Transaction) {
		for _, name := range []string{"heading", "paragraph", "bullet_list", "blockquote", "paragraph", "bullet_list"} {
			h.insertBlock(txn, h.wFrag, h.wFrag.Len(), name, 0)
		}
		h.wSub = NewMapPrelim()
		h.wMeta.Set(txn, "sub", h.wSub)
		h.wMeta.Set(txn, "title", "doc")
	})
	if err := ApplyUpdateV1(h.r, EncodeStateAsUpdateV1(h.w, nil), "seed"); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	sub, ok := h.rMeta.Get("sub")
	if !ok {
		t.Fatal("receiver has no meta.sub after the seed apply")
	}
	h.rSub = sub.(*YMap)
	return h
}

// ── Writer side ──────────────────────────────────────────────────────────────

// insertBlock inserts an element named name into parent at index and fills it
// the way ProseMirror's schema would. depth is the element's depth below the
// fragment (a top-level block is 0).
func (h *ctHarness) insertBlock(txn *Transaction, parent *YXmlFragment, index int, name string, depth int) *YXmlElement {
	e := NewYXmlElement(name)
	parent.InsertElement(txn, index, e)
	switch name {
	case "heading":
		e.SetAttribute(txn, "level", strconv.Itoa(1+h.rng.IntN(3)))
		h.insertText(txn, e)
	case "paragraph":
		h.insertText(txn, e)
	case "bullet_list":
		for n := 1 + h.rng.IntN(2); n > 0; n-- {
			h.insertBlock(txn, &e.YXmlFragment, e.Len(), "list_item", depth+1)
		}
	case "list_item":
		h.insertBlock(txn, &e.YXmlFragment, 0, "paragraph", depth+1)
		if depth < 3 && h.rng.IntN(4) == 0 {
			h.insertBlock(txn, &e.YXmlFragment, 1, "bullet_list", depth+1)
		}
	case "blockquote":
		h.insertBlock(txn, &e.YXmlFragment, 0, "paragraph", depth+1)
		if h.rng.IntN(2) == 0 {
			h.insertBlock(txn, &e.YXmlFragment, 1, "bullet_list", depth+1)
		}
	}
	return e
}

// insertText gives a textblock a YXmlText with one or two formatted runs.
func (h *ctHarness) insertText(txn *Transaction, e *YXmlElement) {
	txt := NewYXmlText()
	e.InsertText(txn, e.Len(), txt)
	for n := 1 + h.rng.IntN(2); n > 0; n-- {
		txt.Insert(txn, txt.Len(), h.word(), h.attr())
	}
}

func (h *ctHarness) word() string { return h.words[h.rng.IntN(len(h.words))] }

func (h *ctHarness) attr() Attributes { return h.attrs[h.rng.IntN(len(h.attrs))] }

// ctNode is a live node of the writer's tree with its depth below the fragment.
type ctNode struct {
	elem  *YXmlElement
	text  *YXmlText
	depth int
}

// writerNodes walks the writer's fragment. Children and Len read the tree
// without the document lock, so this is safe inside the writer's Transact.
func (h *ctHarness) writerNodes() []ctNode {
	var nodes []ctNode
	var walk func(children []xmlNode, depth int)
	walk = func(children []xmlNode, depth int) {
		for _, c := range children {
			switch n := c.(type) {
			case *YXmlElement:
				nodes = append(nodes, ctNode{elem: n, depth: depth})
				walk(n.Children(), depth+1)
			case *YXmlText:
				nodes = append(nodes, ctNode{text: n, depth: depth})
			}
		}
	}
	walk(h.wFrag.Children(), 0)
	return nodes
}

// pick returns a random node that keep accepts, or false when none does.
func (h *ctHarness) pick(keep func(ctNode) bool) (ctNode, bool) {
	var match []ctNode
	for _, n := range h.writerNodes() {
		if keep(n) {
			match = append(match, n)
		}
	}
	if len(match) == 0 {
		return ctNode{}, false
	}
	return match[h.rng.IntN(len(match))], true
}

func isText(n ctNode) bool { return n.text != nil }

func isNonEmptyText(n ctNode) bool { return n.text != nil && n.text.Len() > 0 }

func isElem(names ...string) func(ctNode) bool {
	return func(n ctNode) bool {
		if n.elem == nil {
			return false
		}
		for _, name := range names {
			if n.elem.NodeName == name {
				return true
			}
		}
		return false
	}
}

// randomOp makes one random change to the writer inside txn.
func (h *ctHarness) randomOp(txn *Transaction) {
	switch op := h.rng.IntN(100); {
	case op < 20: // text insert
		if n, ok := h.pick(isText); ok {
			n.text.Insert(txn, h.rng.IntN(n.text.Len()+1), h.word(), h.attr())
			h.touched |= ctTouchFrag
		}
	case op < 32: // text delete
		if n, ok := h.pick(isNonEmptyText); ok {
			idx := h.rng.IntN(n.text.Len())
			n.text.Delete(txn, idx, 1+h.rng.IntN(n.text.Len()-idx))
			h.touched |= ctTouchFrag
		}
	case op < 44: // format a range
		if n, ok := h.pick(isNonEmptyText); ok {
			idx := h.rng.IntN(n.text.Len())
			attrs := h.attr()
			if attrs == nil {
				attrs = Attributes{"bold": true}
			}
			n.text.Format(txn, idx, 1+h.rng.IntN(n.text.Len()-idx), attrs)
			h.touched |= ctTouchFrag
		}
	case op < 54: // element attribute set/delete
		if n, ok := h.pick(func(n ctNode) bool { return n.elem != nil }); ok {
			switch h.rng.IntN(3) {
			case 0:
				n.elem.SetAttribute(txn, "level", strconv.Itoa(1+h.rng.IntN(6)))
			case 1:
				n.elem.SetAttribute(txn, "data-id", h.word())
			default:
				n.elem.DeleteAttribute(txn, []string{"level", "data-id"}[h.rng.IntN(2)])
			}
			h.touched |= ctTouchFrag
		}
	case op < 62: // top-level insert
		names := []string{"paragraph", "heading", "bullet_list", "blockquote"}
		h.insertBlock(txn, h.wFrag, h.rng.IntN(h.wFrag.Len()+1), names[h.rng.IntN(len(names))], 0)
		h.touched |= ctTouchFrag
	case op < 68: // top-level delete
		if n := h.wFrag.Len(); n > 3 {
			h.wFrag.Delete(txn, h.rng.IntN(n), 1)
			h.touched |= ctTouchFrag
		}
	case op < 76: // nested insert
		if n, ok := h.pick(isElem("bullet_list", "list_item", "blockquote")); ok {
			at := h.rng.IntN(n.elem.Len() + 1)
			switch {
			case n.elem.NodeName == "bullet_list":
				h.insertBlock(txn, &n.elem.YXmlFragment, at, "list_item", n.depth+1)
			case n.depth < 3 && h.rng.IntN(3) == 0:
				h.insertBlock(txn, &n.elem.YXmlFragment, at, "bullet_list", n.depth+1)
			default:
				h.insertBlock(txn, &n.elem.YXmlFragment, at, "paragraph", n.depth+1)
			}
			h.touched |= ctTouchFrag
		}
	case op < 82: // nested delete
		if n, ok := h.pick(func(n ctNode) bool {
			return isElem("bullet_list", "list_item", "blockquote")(n) && n.elem.Len() > 1
		}); ok {
			n.elem.Delete(txn, h.rng.IntN(n.elem.Len()), 1)
			h.touched |= ctTouchFrag
		}
	case op < 90: // nested list growth
		if n, ok := h.pick(isElem("bullet_list", "list_item")); ok {
			if n.elem.NodeName == "list_item" && n.depth < 4 {
				h.insertBlock(txn, &n.elem.YXmlFragment, n.elem.Len(), "bullet_list", n.depth+1)
			} else if n.elem.NodeName == "bullet_list" {
				h.insertBlock(txn, &n.elem.YXmlFragment, n.elem.Len(), "list_item", n.depth+1)
			}
			h.touched |= ctTouchFrag
		}
	default: // keyed entries in the other root, top level and nested
		m := h.wMeta
		if h.rng.IntN(2) == 0 {
			m = h.wSub
		}
		key := "k" + strconv.Itoa(h.rng.IntN(4))
		if h.rng.IntN(3) == 0 {
			m.Delete(txn, key)
		} else {
			m.Set(txn, key, h.rng.IntN(1000))
		}
		h.touched |= ctTouchMeta
	}
}

// ── Receiver side ────────────────────────────────────────────────────────────

// renderNode renders a node with everything a block render depends on: names,
// attributes and the formatted runs of every text (ToXML drops formatting).
func renderNode(b *strings.Builder, node xmlNode) {
	switch n := node.(type) {
	case *YXmlElement:
		fmt.Fprintf(b, "<%s %v>", n.NodeName, n.GetAttributes())
		for _, c := range n.Children() {
			renderNode(b, c)
		}
		b.WriteString("</>")
	case *YXmlText:
		fmt.Fprintf(b, "%v", n.ToDelta())
	default:
		b.WriteString(node.ToXML())
	}
}

func renderBlocks(f *YXmlFragment) []string {
	children := f.Children()
	out := make([]string, len(children))
	for i, c := range children {
		var b strings.Builder
		renderNode(&b, c)
		out[i] = b.String()
	}
	return out
}

// walkToRoot returns the types from st up to its root, st first and the root
// (Parent() == nil) last.
func walkToRoot(t *testing.T, st SharedType) []*abstractType {
	t.Helper()
	var path []*abstractType
	for cur := st; cur != nil; cur = cur.Parent() {
		if len(path) == ctMaxWalk {
			t.Fatalf("Parent walk from %T did not reach a root in %d steps", st, ctMaxWalk)
		}
		path = append(path, cur.baseType())
	}
	return path
}

// step makes one writer transaction of one to three operations, applies its
// update to the receiver and checks the contract.
func (h *ctHarness) step(i int) {
	t := h.t
	t.Helper()
	h.update, h.wChanged, h.touched, h.changed = nil, 0, 0, nil
	h.w.Transact(func(txn *Transaction) {
		for n := 1 + h.rng.IntN(3); n > 0; n-- {
			h.randomOp(txn)
		}
	})
	if h.update == nil {
		t.Fatalf("step %d: the writer's transaction produced no update", i)
	}

	beforeLen := h.rFrag.Len()
	before := renderBlocks(h.rFrag)
	if err := ApplyUpdateV1(h.r, h.update, "remote"); err != nil {
		t.Fatalf("step %d: apply: %v", i, err)
	}

	if got, want := strings.Join(renderBlocks(h.rFrag), "\n"), strings.Join(renderBlocks(h.wFrag), "\n"); got != want {
		t.Fatalf("step %d: receiver diverged from writer:\nreceiver:\n%s\nwriter:\n%s", i, got, want)
	}
	if len(h.changed) == 0 {
		// Only an update that changes nothing (a format that matches, the
		// delete of an absent key) may report nothing.
		if h.wChanged > 0 {
			t.Fatalf("step %d: the writer changed %d types but the remote apply reported no ChangedTypes", i, h.wChanged)
		}
		if after := renderBlocks(h.rFrag); strings.Join(after, "\n") != strings.Join(before, "\n") {
			t.Fatalf("step %d: the fragment changed but ChangedTypes is empty", i)
		}
		return
	}

	frag, meta := &h.rFrag.abstractType, &h.rMeta.abstractType
	fragInSet := false
	paths := make([][]*abstractType, len(h.changed))
	for k, st := range h.changed {
		path := walkToRoot(t, st)
		paths[k] = path
		root := path[len(path)-1]
		if root != frag && root != meta {
			t.Fatalf("step %d: ChangedTypes entry %T walks to a root that is neither the fragment nor the map", i, st)
		}
		if path[0] == frag {
			fragInSet = true
		}
		// An entry under the other root never walks to the fragment.
		if st == SharedType(h.rMeta) || st == SharedType(h.rSub) {
			if root != meta {
				t.Fatalf("step %d: map entry %T walks to the fragment", i, st)
			}
		}
		if h.touched == ctTouchMeta && root == frag {
			t.Fatalf("step %d: a map-only transaction reported %T under the fragment", i, st)
		}
		if h.touched == ctTouchFrag && root == meta {
			t.Fatalf("step %d: a fragment-only transaction reported %T under the map", i, st)
		}
	}
	if h.touched == ctTouchMeta {
		h.stats.metaOnly++
	}
	if fragInSet {
		h.stats.fragInSet++
		return
	}

	if n := h.rFrag.Len(); n != beforeLen {
		t.Fatalf("step %d: fragment child count %d -> %d but the fragment is not in ChangedTypes", i, beforeLen, n)
	}
	after := renderBlocks(h.rFrag)
	children := h.rFrag.Children()
	for idx := range after {
		if before[idx] == after[idx] {
			continue
		}
		block := children[idx].baseXMLType()
		reached := false
		for _, path := range paths {
			for _, at := range path {
				if at == block {
					reached = true
					break
				}
			}
			if reached {
				break
			}
		}
		if !reached {
			t.Fatalf("step %d: block %d changed but no ChangedTypes entry walks to it\nbefore: %s\nafter:  %s",
				i, idx, before[idx], after[idx])
		}
		h.stats.blocksAttested++
	}
}

// TestUnit_ChangedTypes_RemoteApplyReachesEveryChangedBlock is the randomized
// contract test: seeded, so every run makes the same operations.
func TestUnit_ChangedTypes_RemoteApplyReachesEveryChangedBlock(t *testing.T) {
	const steps = 250
	for _, seed := range []uint64{1, 2, 3, 0x644} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			h := newCTHarness(t, seed)
			for i := range steps {
				h.step(i)
			}
			// Each branch of the check must have run, or the test proves nothing.
			if h.stats.fragInSet == 0 || h.stats.blocksAttested == 0 || h.stats.metaOnly == 0 {
				t.Fatalf("seed %d exercised too little: %+v", seed, h.stats)
			}
			t.Logf("seed %d: %+v", seed, h.stats)
		})
	}
}

// TestUnit_ChangedTypes_NamesCreatedAndDeletedTypes pins what ChangedTypes reports
// beyond Yjs `transaction.changed`: a type the transaction created (its own
// children integrate into it) and a type it deleted (the delete cascades into
// its children). Both are also reachable from their parent, which is an entry.
func TestUnit_ChangedTypes_NamesCreatedAndDeletedTypes(t *testing.T) {
	d := New(WithClientID(1))
	frag := d.GetXmlFragment("prosemirror")
	var changed []SharedType
	d.OnAfterTransaction(func(txn *Transaction) { changed = txn.ChangedTypes() })

	has := func(st SharedType) bool {
		for _, c := range changed {
			if c == st {
				return true
			}
		}
		return false
	}

	p := NewYXmlElement("paragraph")
	txt := NewYXmlText()
	d.Transact(func(txn *Transaction) {
		frag.InsertElement(txn, 0, p)
		p.InsertText(txn, 0, txt)
		txt.Insert(txn, 0, "hi", nil)
	})
	if !has(frag) || !has(p) || !has(txt) {
		t.Fatalf("insert: ChangedTypes %v misses the fragment, the new paragraph or its text", changed)
	}

	d.Transact(func(txn *Transaction) { frag.Delete(txn, 0, 1) })
	if !has(frag) || !has(p) {
		t.Fatalf("delete: ChangedTypes %v misses the fragment or the deleted paragraph", changed)
	}
}

// TestUnit_ChangedTypes_NamesATypeDeletedBeforeAConcurrentWrite pins the third
// kind of entry Yjs `transaction.changed` leaves out: a type deleted before
// the transaction that a concurrent remote write still reaches. Its own item
// is deleted (the earlier delete cascaded into it), so it no longer appears
// among its Parent's children, which is how a caller filters it.
func TestUnit_ChangedTypes_NamesATypeDeletedBeforeAConcurrentWrite(t *testing.T) {
	w := New(WithClientID(1))
	r := New(WithClientID(2))
	wFrag := w.GetXmlFragment("prosemirror")
	p := NewYXmlElement("paragraph")
	txt := NewYXmlText()
	w.Transact(func(txn *Transaction) {
		wFrag.InsertElement(txn, 0, p)
		p.InsertText(txn, 0, txt)
		txt.Insert(txn, 0, "hi", nil)
	})
	if err := ApplyUpdateV1(r, EncodeStateAsUpdateV1(w, nil), nil); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	rFrag := r.GetXmlFragment("prosemirror")
	rP := rFrag.Children()[0].(*YXmlElement)
	rTxt := rP.Children()[0].(*YXmlText)
	r.Transact(func(txn *Transaction) { rFrag.Delete(txn, 0, 1) })

	var changed []SharedType
	r.OnAfterTransaction(func(txn *Transaction) { changed = txn.ChangedTypes() })
	sv := r.StateVector()
	w.Transact(func(txn *Transaction) { txt.Insert(txn, 2, "!", nil) })
	if err := ApplyUpdateV1(r, EncodeStateAsUpdateV1(w, sv), nil); err != nil {
		t.Fatalf("concurrent write apply: %v", err)
	}
	if !slices.Contains(changed, SharedType(rTxt)) {
		t.Fatalf("ChangedTypes %v misses the text the concurrent write reached", changed)
	}
	if !rTxt.item.Deleted {
		t.Fatal("the text's own item is live after its paragraph was deleted")
	}
	if p := rTxt.Parent(); p != SharedType(rP) {
		t.Fatalf("text Parent() = %T, want its deleted paragraph", p)
	}
	if n := len(rP.Children()); n != 0 {
		t.Fatalf("the deleted paragraph lists %d children, want 0", n)
	}
	if n := rFrag.Len(); n != 0 {
		t.Fatalf("the fragment holds %d children, want 0", n)
	}
}

// TestUnit_ChangedTypes_ParentOfRootsAndDetachedTypes: Parent is nil for a root and
// for a type not yet inserted, and set once it is.
func TestUnit_ChangedTypes_ParentOfRootsAndDetachedTypes(t *testing.T) {
	d := New(WithClientID(1))
	frag := d.GetXmlFragment("prosemirror")
	meta := d.GetMap("meta")
	for _, st := range []SharedType{frag, meta, d.GetText("t"), d.GetArray("a")} {
		if p := st.Parent(); p != nil {
			t.Errorf("root %T: Parent() = %T, want nil", st, p)
		}
	}

	elem := NewYXmlElement("paragraph")
	txt := NewYXmlText()
	sub := NewMapPrelim()
	for _, st := range []SharedType{elem, txt, sub} {
		if p := st.Parent(); p != nil {
			t.Errorf("detached %T: Parent() = %T, want nil", st, p)
		}
	}

	d.Transact(func(txn *Transaction) {
		frag.InsertElement(txn, 0, elem)
		elem.InsertText(txn, 0, txt)
		meta.Set(txn, "sub", sub)
	})
	if p := elem.Parent(); p != SharedType(frag) {
		t.Errorf("inserted element: Parent() = %T, want the fragment", p)
	}
	if p := txt.Parent(); p != SharedType(elem) {
		t.Errorf("inserted text: Parent() = %T, want its element", p)
	}
	if p := sub.Parent(); p != SharedType(meta) {
		t.Errorf("nested map: Parent() = %T, want the root map", p)
	}
}

// TestUnit_ChangedTypes_ParentSurvivesGC: a deleted nested element still names the
// element it was deleted from after the garbage collector freed its container
// item, locally and on a peer that applied the delete remotely.
func TestUnit_ChangedTypes_ParentSurvivesGC(t *testing.T) {
	w := New(WithClientID(1), WithGC(true))
	r := New(WithClientID(2), WithGC(true))
	wFrag := w.GetXmlFragment("prosemirror")

	// blockquote > bullet_list > list_item > paragraph > text
	bq := NewYXmlElement("blockquote")
	list := NewYXmlElement("bullet_list")
	li := NewYXmlElement("list_item")
	p := NewYXmlElement("paragraph")
	txt := NewYXmlText()
	w.Transact(func(txn *Transaction) {
		wFrag.InsertElement(txn, 0, bq)
		bq.InsertElement(txn, 0, list)
		list.InsertElement(txn, 0, li)
		li.InsertElement(txn, 0, p)
		p.InsertText(txn, 0, txt)
		txt.Insert(txn, 0, "gone soon", Attributes{"bold": true})
	})
	if err := ApplyUpdateV1(r, EncodeStateAsUpdateV1(w, nil), nil); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	rLi := r.GetXmlFragment("prosemirror").Children()[0].(*YXmlElement).
		Children()[0].(*YXmlElement).Children()[0].(*YXmlElement)
	rP := rLi.Children()[0].(*YXmlElement)
	rTxt := rP.Children()[0].(*YXmlText)

	sv := r.StateVector()
	w.Transact(func(txn *Transaction) { li.Delete(txn, 0, 1) })
	if err := ApplyUpdateV1(r, EncodeStateAsUpdateV1(w, sv), nil); err != nil {
		t.Fatalf("delete apply: %v", err)
	}

	for _, c := range []struct {
		name  string
		li, p *YXmlElement
		txt   *YXmlText
	}{
		{"local", li, p, txt},
		{"remote", rLi, rP, rTxt},
	} {
		if _, ok := c.p.item.Content.(*ContentDeleted); !ok {
			t.Fatalf("%s: deleted paragraph's container item holds %T, want *ContentDeleted (GC did not run)",
				c.name, c.p.item.Content)
		}
		if got := SharedType(c.p).Parent(); got != SharedType(c.li) {
			t.Errorf("%s: deleted paragraph Parent() = %T %p, want its old list_item %p", c.name, got, got, c.li)
		}
		if got := c.txt.Parent(); got != SharedType(c.p) {
			t.Errorf("%s: text of the deleted paragraph Parent() = %T, want the paragraph", c.name, got)
		}
		if c.li.Len() != 0 {
			t.Errorf("%s: list_item still has %d children", c.name, c.li.Len())
		}
	}
}

// ctNoPlaceholder fails the test when ChangedTypes handed out an internal
// placeholder.
func ctNoPlaceholder(t *testing.T, step string, changed []SharedType) {
	t.Helper()
	for _, c := range changed {
		if _, ok := c.(*rawType); ok {
			t.Fatalf("%s: ChangedTypes %v names an internal placeholder", step, changed)
		}
	}
}

// TestUnit_ChangedTypes_UnopenedRoot: a root the receiver has not opened by name is
// held as an internal placeholder that opening it replaces, so ChangedTypes
// leaves it out and a type directly under it has no Parent. Once Get* opens
// the root, Parent and later ChangedTypes name the type Get* returned.
func TestUnit_ChangedTypes_UnopenedRoot(t *testing.T) {
	w := New(WithClientID(1))
	wFrag := w.GetXmlFragment("prosemirror")
	wMeta := w.GetMap("meta")
	r := New(WithClientID(2))
	var changed []SharedType
	r.OnAfterTransaction(func(txn *Transaction) { changed = txn.ChangedTypes() })
	apply := func(step string) {
		t.Helper()
		changed = nil
		if err := ApplyUpdateV1(r, EncodeStateAsUpdateV1(w, r.StateVector()), nil); err != nil {
			t.Fatalf("%s: apply: %v", step, err)
		}
		ctNoPlaceholder(t, step, changed)
	}

	w.Transact(func(txn *Transaction) {
		p := NewYXmlElement("paragraph")
		wFrag.InsertElement(txn, 0, p)
		txt := NewYXmlText()
		p.InsertText(txn, 0, txt)
		txt.Insert(txn, 0, "hi", nil)
		sub := NewMapPrelim()
		wMeta.Set(txn, "sub", sub)
		sub.Set(txn, "k", 1)
	})
	apply("unopened")
	for _, name := range []string{"prosemirror", "meta"} {
		if _, ok := r.share[name].(*rawType); !ok {
			t.Fatalf("root %q is %T before it is opened, want the *rawType placeholder", name, r.share[name])
		}
	}
	var rP *YXmlElement
	var rTxt *YXmlText
	var rSub *YMap
	for _, c := range changed {
		switch v := c.(type) {
		case *YXmlElement:
			rP = v
		case *YXmlText:
			rTxt = v
		case *YMap:
			rSub = v
		}
	}
	if len(changed) != 3 || rP == nil || rTxt == nil || rSub == nil {
		t.Fatalf("unopened: ChangedTypes %v, want the paragraph, its text and the nested map", changed)
	}
	if p := rP.Parent(); p != nil {
		t.Errorf("unopened: paragraph under an unopened root: Parent() = %T, want nil", p)
	}
	if p := rSub.Parent(); p != nil {
		t.Errorf("unopened: map under an unopened root: Parent() = %T, want nil", p)
	}
	if p := rTxt.Parent(); p != SharedType(rP) {
		t.Errorf("unopened: text Parent() = %T, want its paragraph", p)
	}

	rFrag := r.GetXmlFragment("prosemirror")
	rMeta := r.GetMap("meta")
	if p := rP.Parent(); p != SharedType(rFrag) {
		t.Errorf("opened: paragraph Parent() = %T %p, want the fragment GetXmlFragment returned %p", p, p, rFrag)
	}
	if p := rSub.Parent(); p != SharedType(rMeta) {
		t.Errorf("opened: nested map Parent() = %T %p, want the map GetMap returned %p", p, p, rMeta)
	}

	w.Transact(func(txn *Transaction) {
		wFrag.InsertElement(txn, 1, NewYXmlElement("paragraph"))
		wMeta.Set(txn, "title", "doc")
	})
	apply("after open")
	if !slices.Contains(changed, SharedType(rFrag)) || !slices.Contains(changed, SharedType(rMeta)) {
		t.Errorf("after open: ChangedTypes %v misses the fragment or the map Get* returned", changed)
	}
}

// TestUnit_ChangedTypes_UnknownClassContainer: a nested type of a class ygo does not
// know (a Yjs YXmlHook) is held as an internal placeholder, so ChangedTypes
// leaves it out and a type inside it has no Parent.
func TestUnit_ChangedTypes_UnknownClassContainer(t *testing.T) {
	// Client 1's three structs, none with an origin: a YXmlHook (type class 5)
	// in the root "f", a <div> in the hook and a YXmlText in the div.
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(3) // structs
	enc.WriteVarUint(1) // client
	enc.WriteVarUint(0) // first clock
	enc.WriteUint8(wireType)
	enc.WriteUint8(1) // parent is a root, by name
	enc.WriteVarString("f")
	enc.WriteUint8(5)
	enc.WriteVarString("myhook")
	enc.WriteUint8(wireType)
	enc.WriteUint8(0) // parent is the type of item 1:0
	enc.WriteVarUint(1)
	enc.WriteVarUint(0)
	enc.WriteUint8(3)
	enc.WriteVarString("div")
	enc.WriteUint8(wireType)
	enc.WriteUint8(0) // parent is the type of item 1:1
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteUint8(6)
	enc.WriteVarUint(0) // empty delete set

	r := New(WithClientID(2))
	frag := r.GetXmlFragment("f")
	var changed []SharedType
	r.OnAfterTransaction(func(txn *Transaction) { changed = txn.ChangedTypes() })
	if err := ApplyUpdateV1(r, enc.Bytes(), nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ctNoPlaceholder(t, "hook", changed)
	var div *YXmlElement
	for _, c := range changed {
		if e, ok := c.(*YXmlElement); ok && e.NodeName == "div" {
			div = e
		}
	}
	if len(changed) != 2 || !slices.Contains(changed, SharedType(frag)) || div == nil {
		t.Fatalf("ChangedTypes %v, want the fragment and the div", changed)
	}
	if p := div.Parent(); p != nil {
		t.Errorf("div inside a YXmlHook: Parent() = %T, want nil", p)
	}
}
