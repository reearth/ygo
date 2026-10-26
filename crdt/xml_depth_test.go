package crdt

import (
	"os"
	"os/exec"
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const deepXMLTestDepth = 20_000

type xmlElementInserter interface {
	InsertElement(*Transaction, int, *YXmlElement)
}

// TestUnit_YXmlFragment_ToXML_DeepRemoteNestingIsStackSafe proves that an XML
// read of a remotely received document does not recursively consume the Go
// stack for every nested element.
func TestUnit_YXmlFragment_ToXML_DeepRemoteNestingIsStackSafe(t *testing.T) {
	if os.Getenv("YGO_DEEP_XML_HELPER") == "1" {
		debug.SetMaxStack(256 * 1024)
		applyAndRenderDeepXML()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestUnit_YXmlFragment_ToXML_DeepRemoteNestingIsStackSafe$")
	cmd.Env = append(os.Environ(), "YGO_DEEP_XML_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ToXML on a %d-level remote nested element chain failed: %v\n%s", deepXMLTestDepth, err, output)
	}
}

func applyAndRenderDeepXML() {
	source := newTestDoc(1)
	fragment := source.GetXmlFragment("root")
	var parent xmlElementInserter = fragment
	source.Transact(func(txn *Transaction) {
		for range deepXMLTestDepth {
			element := NewYXmlElement("n")
			parent.InsertElement(txn, 0, element)
			parent = element
		}
	})

	target := newTestDoc(2)
	result := target.GetXmlFragment("root")
	if err := ApplyUpdateV1(target, EncodeStateAsUpdateV1(source, nil), nil); err != nil {
		panic(err)
	}
	if rendered := result.ToXML(); len(rendered) == 0 {
		panic("empty XML result")
	}
}

// renderXML pushes a detached element's buffered children in reverse so that
// it pops them in document order.
func TestUnit_YXmlElement_ToXML_DetachedChildrenInOrder(t *testing.T) {
	list := NewYXmlElement("ul")
	for i, name := range []string{"a", "b", "c"} {
		item := NewYXmlElement(name)
		item.Insert(nil, 0, NewYXmlElement(name+"1"), NewYXmlElement(name+"2"))
		list.Insert(nil, i, item)
	}
	const want = "<ul><a><a1></a1><a2></a2></a><b><b1></b1><b2></b2></b><c><c1></c1><c2></c2></c></ul>"
	if got := list.ToXML(); got != want {
		t.Errorf("ToXML() = %q, want %q", got, want)
	}
	if got := list.toXMLLocked(); got != want {
		t.Errorf("toXMLLocked() = %q, want %q", got, want)
	}
}

// toXMLLocked must stay lock-free all the way down: a nested YXmlText read
// through its unlocked ToXML would deadlock under the write lock.
func TestUnit_YXmlElement_ToXMLLocked_UnderWriteLock(t *testing.T) {
	doc := newTestDoc(1)
	fragment := doc.GetXmlFragment("x")
	element := NewYXmlElement("p")
	text := NewYXmlText()
	doc.Transact(func(txn *Transaction) {
		fragment.InsertElement(txn, 0, element)
		element.InsertText(txn, 0, text)
		text.Insert(txn, 0, "a<b", nil)
	})

	done := make(chan [2]string, 1)
	go func() {
		doc.Transact(func(*Transaction) {
			done <- [2]string{element.toXMLLocked(), fragment.toXMLLocked()}
		})
	}()
	select {
	case got := <-done:
		require.Equal(t, "<p>a&lt;b</p>", got[0])
		require.Equal(t, "<p>a&lt;b</p>", got[1])
	case <-time.After(10 * time.Second):
		t.Fatal("toXMLLocked took the document lock")
	}
}

// With GC off a deleted child keeps its type, which ToXML must still skip.
func TestUnit_YXmlFragment_ToXML_SkipsDeletedChildWithoutGC(t *testing.T) {
	doc := New(WithClientID(1), WithGC(false))
	fragment := doc.GetXmlFragment("x")
	doc.Transact(func(txn *Transaction) {
		fragment.Insert(txn, 0, NewYXmlElement("a"), NewYXmlElement("b"), NewYXmlElement("c"))
	})
	doc.Transact(func(txn *Transaction) { fragment.Delete(txn, 1, 1) })
	require.Equal(t, "<a></a><c></c>", fragment.ToXML())
}
