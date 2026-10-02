package crdt

import (
	"os"
	"os/exec"
	"runtime/debug"
	"testing"
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
