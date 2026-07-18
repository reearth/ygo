package crdt_test

import (
	"testing"

	"github.com/reearth/ygo/crdt"
)

// Detached XML nodes buffer their mutations (prelim content/attrs) and are
// uniformly EMPTY to every reader until the subtree attaches: Len, Children,
// attribute getters and ToXML all see nothing. This mirrors YText in this
// package (a detached YText's Len/ToString ignore pending ops) and keeps the
// read API consistent — previously Len() alone consulted the prelim buffer,
// so a detached node could report Len()==1 while Children() was empty
// (an index loop over Children driven by Len would panic), and a value set
// with SetAttributeValue read back as absent.
func TestYXml_DetachedReadsAreEmptyUntilAttached(t *testing.T) {
	doc := crdt.New()
	frag := doc.GetXmlFragment("f")

	el := crdt.NewYXmlElement("paragraph")
	txt := crdt.NewYXmlText()

	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "hello", nil)
		el.InsertText(txn, 0, txt)
		el.SetAttributeValue(txn, "level", 1)

		// Detached: every reader is empty — buffered writes are invisible.
		if n := el.Len(); n != 0 {
			t.Errorf("detached Len() = %d, want 0 (uniform empty-until-attached)", n)
		}
		if kids := el.Children(); len(kids) != 0 {
			t.Errorf("detached Children() = %d nodes, want 0", len(kids))
		}
		if v, ok := el.GetAttributeValue("level"); ok {
			t.Errorf("detached GetAttributeValue = %v, want absent", v)
		}
		if attrs := el.GetAttributes(); len(attrs) != 0 {
			t.Errorf("detached GetAttributes = %v, want empty", attrs)
		}
		if got, want := el.ToXML(), "<paragraph></paragraph>"; got != want {
			t.Errorf("detached ToXML() = %q, want %q", got, want)
		}
		if n := txt.Len(); n != 0 {
			t.Errorf("detached YXmlText Len() = %d, want 0", n)
		}

		frag.InsertElement(txn, 0, el)
	}, nil)

	// Attached: the buffered writes materialised; all readers agree.
	// (Asserted OUTSIDE the transaction: ToXML on an attached tree recurses
	// into YXmlText.ToString, which takes the doc read lock that Transact
	// still holds inside the callback.)
	if n := el.Len(); n != 1 {
		t.Errorf("attached Len() = %d, want 1", n)
	}
	if kids := el.Children(); len(kids) != 1 {
		t.Errorf("attached Children() = %d nodes, want 1", len(kids))
	}
	// NewContentAny normalizes Go ints to int64 (wire-number parity).
	if v, ok := el.GetAttributeValue("level"); !ok || v != int64(1) {
		t.Errorf("attached GetAttributeValue = %v (%T), %v; want int64(1), true", v, v, ok)
	}
	if got, want := el.ToXML(), `<paragraph level="1">hello</paragraph>`; got != want {
		t.Errorf("attached ToXML() = %q, want %q", got, want)
	}
	if n := frag.Len(); n != 1 {
		t.Errorf("root fragment Len() = %d, want 1", n)
	}
	if got, want := frag.ToXML(), `<paragraph level="1">hello</paragraph>`; got != want {
		t.Errorf("final ToXML() = %q, want %q", got, want)
	}
}
