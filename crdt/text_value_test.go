package crdt

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
)

type textValueColor string

type textValueBlob []byte

type textValuePoint struct {
	X int     `json:"x"`
	Y float64 `json:"y,omitempty"`
}

// textValueVias inserts v as an embed or attribute value through every YText
// entry point; vias marked embed store v itself as the inserted value.
var textValueVias = []struct {
	name  string
	embed bool
	call  func(x *YText, txn *Transaction, v any)
}{
	{"InsertEmbed", true, func(x *YText, txn *Transaction, v any) { x.InsertEmbed(txn, 1, v, nil) }},
	{"ApplyDelta embed", true, func(x *YText, txn *Transaction, v any) {
		x.ApplyDelta(txn, []Delta{{Op: DeltaOpRetain, Retain: 1}, {Op: DeltaOpInsert, Insert: v}})
	}},
	{"InsertEmbed attr", false, func(x *YText, txn *Transaction, v any) { x.InsertEmbed(txn, 1, "e", Attributes{"k": v}) }},
	{"Insert attr", false, func(x *YText, txn *Transaction, v any) { x.Insert(txn, 1, "z", Attributes{"k": v}) }},
	{"Format", false, func(x *YText, txn *Transaction, v any) { x.Format(txn, 0, 1, Attributes{"k": v}) }},
	{"ApplyDelta attr", false, func(x *YText, txn *Transaction, v any) {
		x.ApplyDelta(txn, []Delta{{Op: DeltaOpRetain, Retain: 1, Attributes: Attributes{"k": v}}})
	}},
}

// textValueOf returns the embed or "k" attribute value in txt's delta.
func textValueOf(t *testing.T, txt *YText, embed bool) any {
	t.Helper()
	for _, d := range txt.ToDelta() {
		if embed {
			if _, isText := d.Insert.(string); !isText {
				return d.Insert
			}
		} else if v, ok := d.Attributes["k"]; ok {
			return v
		}
	}
	t.Fatalf("no value in %v", txt.ToDelta())
	return nil
}

// A Go value json.Marshal encodes but WriteAny does not (json.Number, typed
// slices and maps, structs, pointers) is stored in its lib0-Any form, so V1
// and V2 both encode the document and a V2 peer reads what the writer does.
func TestUnit_TextValue_NormalisedForV1AndV2(t *testing.T) {
	nan := math.NaN()
	x := 7
	for _, c := range []struct {
		name string
		in   any
		want any
	}{
		{"json.Number int", json.Number("12"), int64(12)},
		{"json.Number float", json.Number("1.5"), 1.5},
		{"json.Number overflow", json.Number("1e400"), math.Inf(1)},
		{"int32", int32(3), int64(3)},
		{"uint8", uint8(4), int64(4)},
		{"uint64 above int64", uint64(1<<63 | 1<<39), float64(1<<63 | 1<<39)},
		{"[]uint64 above int64", []uint64{1<<63 | 1<<39}, []any{float64(1<<63 | 1<<39)}},
		{"named string in map", map[string]textValueColor{"c": "red"}, map[string]any{"c": "red"}},
		{"map[string]string", map[string]string{"a": "b"}, map[string]any{"a": "b"}},
		{"[]string", []string{"a", "b"}, []any{"a", "b"}},
		{"[2]int", [2]int{1, 2}, []any{int64(1), int64(2)}},
		{"[]float64 NaN", []float64{nan, 2}, []any{nan, float64(2)}},
		{"map[string]float64 Inf", map[string]float64{"w": math.Inf(-1)}, map[string]any{"w": math.Inf(-1)}},
		{"nested typed", map[string]any{"l": []int{1}, "n": json.Number("2")}, map[string]any{"l": []any{int64(1)}, "n": int64(2)}},
		{"map[int]string", map[int]string{1: "a"}, map[string]any{"1": "a"}},
		{"struct", textValuePoint{X: 1}, map[string]any{"x": int64(1)}},
		{"*struct", &textValuePoint{X: 2, Y: 0.5}, map[string]any{"x": int64(2), "y": 0.5}},
		{"*int", &x, int64(7)},
		{"nil *struct", (*textValuePoint)(nil), nil},
		{"json.RawMessage", json.RawMessage(`{"a":[1,"s"]}`), map[string]any{"a": []any{int64(1), "s"}}},
		{"[]byte", []byte{1, 2}, []byte{1, 2}},
		{"named []byte", textValueBlob{3}, []byte{3}},
	} {
		for _, via := range textValueVias {
			if via.embed && (c.want == nil || reflect.TypeOf(c.want).Kind() == reflect.String) {
				continue // a nil or string insert is not an embed
			}
			t.Run(c.name+"/"+via.name, func(t *testing.T) {
				doc := newTestDoc(1)
				txt := doc.GetText("t")
				doc.Transact(func(txn *Transaction) {
					txt.Insert(txn, 0, "ab", nil)
					via.call(txt, txn, c.in)
				})
				if c.want == nil {
					// A nil attribute is no attribute.
					for _, d := range txt.ToDelta() {
						if _, ok := d.Attributes["k"]; ok {
							t.Fatalf("delta %v keeps a nil attribute", txt.ToDelta())
						}
					}
				} else if got := textValueOf(t, txt, via.embed); fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", c.want) {
					t.Fatalf("stored %#v, want %#v", got, c.want)
				}
				v2 := New()
				if err := ApplyUpdateV2(v2, EncodeStateAsUpdateV2(doc, nil), nil); err != nil {
					t.Fatal(err)
				}
				if got, want := fmt.Sprint(v2.GetText("t").ToDelta()), fmt.Sprint(txt.ToDelta()); got != want {
					t.Errorf("V2 reload\n got=%s\nwant=%s", got, want)
				}
				if err := ApplyUpdateV1(New(), EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// A shared type, Doc or unencodable value is rejected wherever it sits,
// typed containers and struct fields included, before anything is written.
func TestUnit_TextValue_RejectsUnencodable(t *testing.T) {
	m := New().GetMap("m")
	cyclic := []any{nil}
	cyclic[0] = cyclic
	deep := any("leaf")
	for range maxTextValueDepth + 1 {
		deep = []any{deep}
	}
	for _, c := range []struct {
		name, want string
		in         any
	}{
		{"[]*YMap", "shared type", []*YMap{m}},
		{"map[string]*YMap", "shared type", map[string]*YMap{"m": m}},
		{"struct field", "shared type", struct{ M *YMap }{m}},
		{"struct []any field", "shared type", struct{ L []any }{[]any{NewTextPrelim()}}},
		{"map[int]*YMap", "shared type", map[int]*YMap{1: m}},
		{"*Doc", "Doc", New()},
		{"[]func", "not JSON-encodable", []func(){func() {}}},
		{"map[string]chan", "not JSON-encodable", map[string]chan int{"c": nil}},
		{"struct NaN", "not JSON-encodable", textValuePoint{Y: math.NaN()}},
		{"bad json.Number", "not JSON-encodable", json.Number("x")},
		{"cycle", "not JSON-encodable", cyclic},
		{"too deep", "not JSON-encodable", deep},
	} {
		for _, via := range textValueVias {
			t.Run(c.name+"/"+via.name, func(t *testing.T) {
				doc := newTestDoc(1)
				txt := doc.GetText("t")
				doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "ab", nil) })
				before := EncodeStateAsUpdateV1(doc, nil)
				doc.Transact(func(txn *Transaction) {
					mustPanicContaining(t, c.want, func() { via.call(txt, txn, c.in) })
				})
				if got := EncodeStateAsUpdateV1(doc, nil); !reflect.DeepEqual(got, before) {
					t.Fatal("rejected call wrote to the document")
				}
			})
		}
	}
}

// The deepest value ReadAny accepts inserts and reloads over V2.
func TestUnit_TextValue_MaxDepthReloads(t *testing.T) {
	deep := any("leaf")
	for range maxTextValueDepth {
		deep = []any{deep}
	}
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.InsertEmbed(txn, 0, deep, nil) })
	if err := ApplyUpdateV2(New(), EncodeStateAsUpdateV2(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
}
