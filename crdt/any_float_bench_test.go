package crdt

// Each reader benchmark below runs on a "writer" document built in Go and on a
// "replica" that applied the writer's V2 update, so the replica's numbers
// come from Any decoding.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const (
	replicaBenchSize     = 10_000 // values per ToJSON fixture, as in tojson_bench_test.go
	replicaBenchElements = 1_000
	replicaBenchTextRuns = 1_000
	// replicaBenchTS is a millisecond timestamp a float32 holds exactly; adding a
	// multiple of 1<<17 keeps it exact below 2^41.
	replicaBenchTS = int64(1718999973888)
)

func replicaBenchRecord(i int) any {
	return map[string]any{
		"id":    int64(i),
		"name":  "item",
		"price": float64(i) + 0.25,
		"ok":    i%2 == 0,
		"ts":    replicaBenchTS + int64(i)<<17,
		"tags":  []any{"a", "b", int64(i)},
		"pos":   map[string]any{"x": float64(i) + 0.5, "y": float64(i) + 0.75},
	}
}

var replicaBenchShapes = []struct {
	name  string
	value func(i int) any
}{
	{"nested", replicaBenchRecord},
	{"scalar", func(i int) any { return float64(i) + 0.5 }},
}

func replicaOf(tb testing.TB, writer *Doc) *Doc {
	replica := newTestDoc(2)
	if err := ApplyUpdateV2(replica, EncodeStateAsUpdateV2(writer, nil), nil); err != nil {
		tb.Fatal(err)
	}
	return replica
}

func replicaBenchArrayDoc(value func(int) any) *Doc {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	vals := make([]any, replicaBenchSize)
	for i := range vals {
		vals[i] = value(i)
	}
	doc.Transact(func(txn *Transaction) { arr.Push(txn, vals) })
	return doc
}

func replicaBenchMapDoc(value func(int) any) *Doc {
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		for i := range replicaBenchSize {
			m.Set(txn, fmt.Sprintf("k%05d", i), value(i))
		}
	})
	return doc
}

func replicaBenchXMLDoc() *Doc {
	doc := newTestDoc(1)
	frag := doc.GetXmlFragment("x")
	doc.Transact(func(txn *Transaction) {
		for i := range replicaBenchElements {
			el := NewYXmlElement("p")
			el.SetAttributeValue(txn, "class", "para")
			el.SetAttributeValue(txn, "level", int64(i%6+1))
			el.SetAttributeValue(txn, "width", float64(i)+0.5)
			el.SetAttributeValue(txn, "visible", i%2 == 0)
			el.SetAttributeValue(txn, "ts", replicaBenchTS+int64(i)<<17)
			frag.InsertElement(txn, i, el)
		}
	})
	return doc
}

func replicaBenchTextDoc() *Doc {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		for i := range replicaBenchTextRuns {
			var attrs Attributes
			switch i % 4 {
			case 0:
				attrs = Attributes{"bold": true}
			case 1:
				attrs = Attributes{"color": "#336699"}
			case 2:
				attrs = Attributes{"size": float64(i%20) + 0.5}
			case 3:
				attrs = Attributes{"comment": map[string]any{"id": int64(i), "ts": replicaBenchTS + int64(i)<<17}}
			}
			txt.Insert(txn, txt.Len(), "word ", attrs)
			if i%10 == 9 {
				txt.InsertEmbed(txn, txt.Len(), map[string]any{
					"image": "https://example.com/i.png", "width": 640.5, "height": 480.25,
					"ts": replicaBenchTS + int64(i)<<17,
				}, nil)
			}
		}
	})
	return doc
}

func replicaBenchElementsOf(frag *YXmlFragment) []*YXmlElement {
	children := frag.Children()
	els := make([]*YXmlElement, 0, len(children))
	for _, n := range children {
		els = append(els, n.(*YXmlElement))
	}
	return els
}

func BenchmarkYArray_ToJSON_Replica(b *testing.B) {
	for _, shape := range replicaBenchShapes {
		w := replicaBenchArrayDoc(shape.value)
		r := replicaOf(b, w)
		for _, c := range []struct {
			name string
			arr  *YArray
		}{{"writer", w.GetArray("a")}, {"replica", r.GetArray("a")}} {
			b.Run(shape.name+"/"+c.name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if _, err := c.arr.ToJSON(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkYMap_ToJSON_Replica(b *testing.B) {
	for _, shape := range replicaBenchShapes {
		w := replicaBenchMapDoc(shape.value)
		r := replicaOf(b, w)
		for _, c := range []struct {
			name string
			m    *YMap
		}{{"writer", w.GetMap("m")}, {"replica", r.GetMap("m")}} {
			b.Run(shape.name+"/"+c.name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if _, err := c.m.ToJSON(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkYXmlElement_GetAttributes_Replica(b *testing.B) {
	w := replicaBenchXMLDoc()
	r := replicaOf(b, w)
	for _, c := range []struct {
		name string
		doc  *Doc
	}{{"writer", w}, {"replica", r}} {
		els := replicaBenchElementsOf(c.doc.GetXmlFragment("x"))
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				for _, el := range els {
					if len(el.GetAttributes()) != 5 {
						b.Fatal("attributes missing")
					}
				}
			}
		})
	}
}

func BenchmarkYXmlFragment_ToXML_Replica(b *testing.B) {
	w := replicaBenchXMLDoc()
	r := replicaOf(b, w)
	for _, c := range []struct {
		name string
		doc  *Doc
	}{{"writer", w}, {"replica", r}} {
		frag := c.doc.GetXmlFragment("x")
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if frag.ToXML() == "" {
					b.Fatal("empty XML")
				}
			}
		})
	}
}

// BenchmarkEncodeStateAsUpdateV1_Replica writes every text attribute and embed
// as JSON text (fmtValToJSON).
func BenchmarkEncodeStateAsUpdateV1_Replica(b *testing.B) {
	w := replicaBenchTextDoc()
	r := replicaOf(b, w)
	for _, c := range []struct {
		name string
		doc  *Doc
	}{{"writer", w}, {"replica", r}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if len(EncodeStateAsUpdateV1(c.doc, nil)) == 0 {
					b.Fatal("empty update")
				}
			}
		})
	}
}

var replicaBenchApplyDocs = []struct {
	name string
	doc  func() *Doc
}{
	{"records", func() *Doc { return replicaBenchArrayDoc(replicaBenchRecord) }},
	{"xml", replicaBenchXMLDoc},
	{"text", replicaBenchTextDoc},
}

var replicaBenchVersions = []struct {
	name   string
	encode func(*Doc, StateVector) []byte
	apply  func(*Doc, []byte, any) error
}{
	{"V1", EncodeStateAsUpdateV1, ApplyUpdateV1},
	{"V2", EncodeStateAsUpdateV2, ApplyUpdateV2},
}

func replicaBenchFixturePath(doc, version string) string {
	dir := os.Getenv("YGO_REPLICA_BENCH_FIXTURES")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, doc+"-"+version+".bin")
}

// TestReplicaBench_WriteFixtures writes the "fixed" updates into
// $YGO_REPLICA_BENCH_FIXTURES when $YGO_WRITE_REPLICA_BENCH_FIXTURES is set, so
// that two revisions can decode the same bytes.
func TestReplicaBench_WriteFixtures(t *testing.T) {
	if os.Getenv("YGO_WRITE_REPLICA_BENCH_FIXTURES") == "" {
		t.Skip("YGO_WRITE_REPLICA_BENCH_FIXTURES not set")
	}
	for _, d := range replicaBenchApplyDocs {
		doc := d.doc()
		for _, v := range replicaBenchVersions {
			if err := os.WriteFile(replicaBenchFixturePath(d.name, v.name), v.encode(doc, nil), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// BenchmarkApplyUpdate_Replica applies the update this revision encodes ("own")
// and, when $YGO_REPLICA_BENCH_FIXTURES is set, the update read from there
// ("fixed").
func BenchmarkApplyUpdate_Replica(b *testing.B) {
	for _, d := range replicaBenchApplyDocs {
		doc := d.doc()
		for _, v := range replicaBenchVersions {
			updates := []struct {
				name string
				u    []byte
			}{{"own", v.encode(doc, nil)}}
			if p := replicaBenchFixturePath(d.name, v.name); p != "" {
				u, err := os.ReadFile(p)
				if err != nil {
					b.Fatal(err)
				}
				updates = append(updates, struct {
					name string
					u    []byte
				}{"fixed", u})
			}
			for _, u := range updates {
				b.Run(d.name+"/"+v.name+"/"+u.name, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(u.u)))
					for range b.N {
						if err := v.apply(newTestDoc(9), u.u, nil); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
