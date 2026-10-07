package crdt

import "testing"

// An XML node attaches once, as maps and arrays do: staging it into two
// detached parents, twice into one, or integrating a node attached or staged
// elsewhere fails at the call.

func TestUnit_XMLStage_TwoDetachedParents_PanicsAtSecondCall(t *testing.T) {
	doc := newTestDoc(1)
	doc.Transact(func(txn *Transaction) {
		a, b := NewYXmlElement("a"), NewYXmlElement("b")
		n := NewYXmlText()
		a.InsertText(txn, 0, n)
		mustPanicContaining(t, "Insert: this node is already staged on another parent", func() { b.InsertText(txn, 0, n) })
		if b.Len() != 0 {
			t.Fatalf("rejected node was buffered: Len=%d", b.Len())
		}
	})
}

func TestUnit_XMLStage_SameParentTwice_Panics(t *testing.T) {
	doc := newTestDoc(1)
	doc.Transact(func(txn *Transaction) {
		a, n := NewYXmlElement("a"), NewYXmlElement("n")
		a.InsertElement(txn, 0, n)
		mustPanicContaining(t, "Insert: this node is already staged on this parent", func() { a.InsertElement(txn, 1, n) })

		b, m := NewYXmlElement("b"), NewYXmlElement("m")
		mustPanicContaining(t, "Insert: a node is passed twice", func() { b.Insert(txn, 0, m, m) })
		if b.Len() != 0 || m.stagedOn != nil {
			t.Fatalf("rejected call left state: Len=%d stagedOn=%v", b.Len(), m.stagedOn)
		}
	})
}

// A call rejected on a later node claims none of the earlier ones.
func TestUnit_XMLStage_RejectedCallClaimsNothing(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("f")
	doc.Transact(func(txn *Transaction) {
		a, x := NewYXmlElement("a"), NewYXmlElement("x")
		mustPanicContaining(t, "create a cycle", func() { a.Insert(txn, 0, x, a) })
		if a.Len() != 0 || x.stagedOn != nil {
			t.Fatalf("rejected call left state: Len=%d stagedOn=%v", a.Len(), x.stagedOn)
		}
		b := NewYXmlElement("b")
		b.InsertElement(txn, 0, x)
		frag.InsertElement(txn, 0, b)
	})
	if got, want := frag.ToXML(), "<b><x></x></b>"; got != want {
		t.Fatalf("ToXML = %q, want %q", got, want)
	}
}

// Deleting a buffered child releases it; attaching clears every claim.
func TestUnit_XMLStage_DeleteReleases_AttachClears(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("f")
	a, b, n := NewYXmlElement("a"), NewYXmlElement("b"), NewYXmlElement("n")
	leaf := NewYXmlText()
	doc.Transact(func(txn *Transaction) {
		n.InsertText(txn, 0, leaf)
		a.InsertElement(txn, 0, n)
		a.Delete(txn, 0, 1)
		b.InsertElement(txn, 0, n)
		frag.Insert(txn, 0, a, b)
	})
	for name, bt := range map[string]*abstractType{"a": &a.abstractType, "b": &b.abstractType, "n": &n.abstractType, "leaf": &leaf.abstractType} {
		if bt.stagedOn != nil {
			t.Errorf("%s: claim survived attach", name)
		}
	}
	if got, want := frag.ToXML(), "<a></a><b><n></n></b>"; got != want {
		t.Fatalf("ToXML = %q, want %q", got, want)
	}
	assertXMLRoundTrips(t, doc, "<a></a><b><n></n></b>")
}

func TestUnit_XMLStage_AttachedParent_RejectsClaimedOrAttachedNode(t *testing.T) {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("f")
	a, n := NewYXmlElement("a"), NewYXmlElement("n")
	doc.Transact(func(txn *Transaction) {
		a.InsertElement(txn, 0, n)
		mustPanicContaining(t, "Insert: this node is staged on another parent", func() { frag.InsertElement(txn, 0, n) })
		frag.InsertElement(txn, 0, a)
		mustPanicContaining(t, "Insert requires a detached node", func() { frag.InsertElement(txn, 1, n) })
		mustPanicContaining(t, "Insert requires a detached node", func() { NewYXmlElement("c").InsertElement(txn, 0, n) })
	})
	if got, want := frag.ToXML(), "<a><n></n></a>"; got != want {
		t.Fatalf("ToXML = %q, want %q", got, want)
	}
	assertXMLRoundTrips(t, doc, "<a><n></n></a>")
}

// An XML node staged in a map is spoken for, too.
func TestUnit_XMLStage_StagedInMap_RejectedByXML(t *testing.T) {
	doc := newTestDoc(1)
	doc.Transact(func(txn *Transaction) {
		m, n := NewMapPrelim(), NewYXmlElement("n")
		m.Set(txn, "k", n)
		mustPanicContaining(t, "Insert: this node is already staged on another parent", func() { NewYXmlElement("a").InsertElement(txn, 0, n) })
	})
}

// assertXMLRoundTrips applies doc's V1 and V2 state to fresh docs and checks
// fragment "f" reads want in each.
func assertXMLRoundTrips(t *testing.T, doc *Doc, want string) {
	t.Helper()
	for tag, c := range map[string]struct {
		b     []byte
		apply func(*Doc, []byte, any) error
	}{"v1": {EncodeStateAsUpdateV1(doc, nil), ApplyUpdateV1}, "v2": {EncodeStateAsUpdateV2(doc, nil), ApplyUpdateV2}} {
		d := New()
		if err := c.apply(d, c.b, nil); err != nil {
			t.Fatalf("%s apply: %v", tag, err)
		}
		if got := d.GetXmlFragment("f").ToXML(); got != want {
			t.Fatalf("%s ToXML = %q, want %q", tag, got, want)
		}
	}
}
