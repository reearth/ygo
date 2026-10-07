package crdt

import (
	"math"
	"reflect"
	"testing"
)

// #283: V1 writes format attributes and embeds as JSON text. A value JSON
// cannot encode used to be written as "null", silently. The text entry points
// now reject it at the call; a non-finite number that arrives from a V2 peer
// re-encodes as JSON.stringify does, null in place.

func TestUnit_JSONValue_TextEntryPointsReject(t *testing.T) {
	nan := math.NaN()
	for _, c := range []struct {
		name string
		call func(txt *YText, txn *Transaction)
	}{
		{"Insert attr NaN", func(x *YText, txn *Transaction) { x.Insert(txn, 0, "a", Attributes{"w": nan}) }},
		{"Insert attr nested Inf", func(x *YText, txn *Transaction) {
			x.Insert(txn, 0, "a", Attributes{"w": map[string]any{"k": []any{1, math.Inf(1)}}})
		}},
		{"InsertEmbed NaN", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, nan, nil) }},
		{"InsertEmbed float32 Inf", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, float32(math.Inf(-1)), nil) }},
		{"InsertEmbed func", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, map[string]any{"f": func() {}}, nil) }},
		{"InsertEmbed chan", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, make(chan int), nil) }},
		{"InsertEmbed attr", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, "img", Attributes{"w": nan}) }},
		{"Format", func(x *YText, txn *Transaction) { x.Format(txn, 0, 1, Attributes{"w": nan}) }},
		{"ApplyDelta embed", func(x *YText, txn *Transaction) {
			x.ApplyDelta(txn, []Delta{{Op: DeltaOpInsert, Insert: "ok"}, {Op: DeltaOpInsert, Insert: nan}})
		}},
		{"ApplyDelta attr", func(x *YText, txn *Transaction) {
			x.ApplyDelta(txn, []Delta{{Op: DeltaOpInsert, Insert: "ok", Attributes: Attributes{"w": nan}}})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := newTestDoc(1)
			txt := doc.GetText("t")
			doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "x", nil) })
			before := EncodeStateAsUpdateV1(doc, nil)
			doc.Transact(func(txn *Transaction) {
				mustPanicContaining(t, "not JSON-encodable", func() { c.call(txt, txn) })
			})
			if got := EncodeStateAsUpdateV1(doc, nil); !reflect.DeepEqual(got, before) {
				t.Fatal("rejected call wrote to the document")
			}
			// A detached text rejects at the call, not at attach.
			doc.Transact(func(txn *Transaction) {
				mustPanicContaining(t, "not JSON-encodable", func() { c.call(NewTextPrelim(), txn) })
			})
		})
	}
}

func TestUnit_JSONValue_FiniteAndPlainValuesAccepted(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "ab", Attributes{"bold": true, "size": 1.5, "n": int64(3), "o": map[string]any{"k": []any{"v", nil}}})
		txt.InsertEmbed(txn, 1, map[string]any{"src": "x.png", "w": float32(2)}, nil)
		txt.Format(txn, 0, 1, Attributes{"bold": nil})
	})
}

func TestUnit_FmtValToJSON_NonFiniteAsJSONStringify(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{
		{math.NaN(), "null"},
		{float32(math.Inf(1)), "null"},
		{[]any{1, math.Inf(-1), "s"}, `[1,null,"s"]`},
		{map[string]any{"a": math.NaN(), "b": 1.5}, `{"a":null,"b":1.5}`},
	} {
		if got := fmtValToJSON(c.in); got != c.want {
			t.Errorf("fmtValToJSON(%v) = %s, want %s", c.in, got, c.want)
		}
	}
	mustPanicContaining(t, "not JSON-encodable", func() { fmtValToJSON(func() {}) })
}

// A non-finite embed can only arrive over V2 (lib0 floats); re-encoding it
// as V1 nulls just that number, as Yjs's JSON.stringify does.
func TestUnit_JSONValue_V2NaNEmbedReencodesV1(t *testing.T) {
	src := newTestDoc(1)
	txt := src.GetText("t")
	src.Transact(func(txn *Transaction) {
		item := &Item{
			ID:      ID{Client: src.clientID, Clock: src.store.NextClock(src.clientID)},
			Parent:  &txt.abstractType,
			Content: NewContentEmbed(map[string]any{"w": math.NaN(), "src": "x"}),
		}
		item.integrate(txn, 0)
	})
	peer := New()
	if err := ApplyUpdateV2(peer, EncodeStateAsUpdateV2(src, nil), nil); err != nil {
		t.Fatal(err)
	}
	out := New()
	if err := ApplyUpdateV1(out, EncodeStateAsUpdateV1(peer, nil), nil); err != nil {
		t.Fatal(err)
	}
	d := out.GetText("t").ToDelta()
	if len(d) != 1 || !reflect.DeepEqual(d[0].Insert, map[string]any{"w": nil, "src": "x"}) {
		t.Fatalf("delta = %#v", d)
	}
}
