package crdt

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
)

// cloneBench holds the large document both copies are measured on, built
// once: a prosemirror-shaped YXmlFragment of several hundred paragraphs, each
// an element with two attributes holding one YXmlText with marks over it.
var cloneBench struct {
	once sync.Once
	doc  *Doc
	size int
}

// cloneBenchDoc returns the shared large document and the size of its V1
// encoding, which the copy under BenchmarkDocCopy_EncodeApply pays for.
func cloneBenchDoc() (*Doc, int) {
	cloneBench.once.Do(func() {
		words := strings.Fields("the quick brown fox jumps over the lazy dog while " +
			"a collaborative editor merges every keystroke into one shared document")
		rng := rand.New(rand.NewPCG(42, 7))
		d := New(WithClientID(1))
		frag := d.GetXmlFragment("prosemirror")
		for p := range 400 {
			var sb strings.Builder
			for sb.Len() < 700 {
				sb.WriteString(words[rng.IntN(len(words))])
				sb.WriteByte(' ')
			}
			text := sb.String()
			d.Transact(func(tx *Transaction) {
				el := NewYXmlElement("paragraph")
				frag.InsertElement(tx, p, el)
				el.SetAttribute(tx, "id", fmt.Sprintf("p-%d", p))
				el.SetAttributeValue(tx, "indent", p%4)
				txt := NewYXmlText()
				el.InsertText(tx, 0, txt)
				txt.Insert(tx, 0, text, nil)
				for _, mark := range []Attributes{
					{"bold": true}, {"italic": true}, {"link": "https://example.test"}, {"code": true},
				} {
					at := rng.IntN(len(text) - 60)
					txt.Format(tx, at, 20+rng.IntN(40), mark)
				}
			})
		}
		cloneBench.doc = d
		cloneBench.size = len(EncodeStateAsUpdateV1(d, nil))
	})
	return cloneBench.doc, cloneBench.size
}

// BenchmarkDocCopy_Clone copies the large document with Doc.Clone.
func BenchmarkDocCopy_Clone(b *testing.B) {
	doc, _ := cloneBenchDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if cp := doc.Clone(WithClientID(2)); cp == nil {
			b.Fatal("nil clone")
		}
	}
}

// BenchmarkDocCopy_EncodeApply copies the same document the way a caller
// without Clone has to: encode its whole state and apply it to a fresh
// document.
func BenchmarkDocCopy_EncodeApply(b *testing.B) {
	doc, _ := cloneBenchDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		cp := New(WithClientID(2))
		if err := ApplyUpdateV1(cp, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
			b.Fatal(err)
		}
	}
}

// TestCloneBenchDocSize guards the benchmark fixture: the document the two
// copies are measured on is the few-hundred-paragraph, ~400 KB document the
// numbers in CHANGELOG.md were taken on, and a change that shrinks it would
// quietly change what the benchmark measures.
func TestCloneBenchDocSize(t *testing.T) {
	doc, size := cloneBenchDoc()
	t.Logf("benchmark document: %d bytes encoded, %d paragraphs", size, doc.GetXmlFragment("prosemirror").Len())
	if size < 350_000 || size > 450_000 {
		t.Fatalf("benchmark document encodes to %d bytes, want ~400 KB", size)
	}
}
