package crdt

import (
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"testing"
)

// V1 writes format attributes and embeds as JSON text: values with no JSON
// form panic at the call, and a non-finite number is written as null, as
// JSON.stringify does.

func TestUnit_JSONValue_TextEntryPointsReject(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(txt *YText, txn *Transaction)
	}{
		{"Insert attr func", func(x *YText, txn *Transaction) { x.Insert(txn, 0, "a", Attributes{"w": func() {}}) }},
		{"Insert attr nested chan", func(x *YText, txn *Transaction) {
			x.Insert(txn, 0, "a", Attributes{"w": map[string]any{"k": []any{1, make(chan int)}}})
		}},
		{"InsertEmbed func", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, map[string]any{"f": func() {}}, nil) }},
		{"InsertEmbed chan", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, make(chan int), nil) }},
		{"InsertEmbed complex", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, complex(1, 2), nil) }},
		{"InsertEmbed attr", func(x *YText, txn *Transaction) { x.InsertEmbed(txn, 0, "img", Attributes{"w": func() {}}) }},
		{"Format", func(x *YText, txn *Transaction) { x.Format(txn, 0, 1, Attributes{"w": make(chan int)}) }},
		{"ApplyDelta embed", func(x *YText, txn *Transaction) {
			x.ApplyDelta(txn, []Delta{{Op: DeltaOpInsert, Insert: "ok"}, {Op: DeltaOpInsert, Insert: func() {}}})
		}},
		{"ApplyDelta attr", func(x *YText, txn *Transaction) {
			x.ApplyDelta(txn, []Delta{{Op: DeltaOpInsert, Insert: "ok", Attributes: Attributes{"w": complex(1, 0)}}})
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

// Non-finite numbers are accepted at every text entry point; V2 keeps them
// and V1 writes each as null, so a V1 peer reads what Yjs's would.
func TestUnit_JSONValue_NonFiniteAccepted(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "abcd", Attributes{"w": nan})
		txt.Format(txn, 1, 1, Attributes{"h": map[string]any{"k": []any{1, math.Inf(-1)}}})
		txt.InsertEmbed(txn, 4, float32(inf), Attributes{"e": nan})
		txt.ApplyDelta(txn, []Delta{{Op: DeltaOpRetain, Retain: 5}, {Op: DeltaOpInsert, Insert: "z", Attributes: Attributes{"d": []any{float32(nan)}}}})
	})
	prelim := NewTextPrelim()
	prelim.Insert(nil, 0, "p", Attributes{"w": nan})
	prelim.InsertEmbed(nil, 1, map[string]any{"v": inf}, nil)

	v2 := New()
	if err := ApplyUpdateV2(v2, EncodeStateAsUpdateV2(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(v2.GetText("t").ToDelta()), fmt.Sprint(txt.ToDelta()); got != want {
		t.Errorf("V2 reload\n got=%s\nwant=%s", got, want)
	}
	v1 := New()
	if err := ApplyUpdateV1(v1, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	// A null attribute is no attribute, as for a Yjs V1 peer.
	want := []Delta{
		{Op: DeltaOpInsert, Insert: "a"},
		{Op: DeltaOpInsert, Insert: "b", Attributes: Attributes{"h": map[string]any{"k": []any{float64(1), nil}}}},
		{Op: DeltaOpInsert, Insert: "cd"},
		{Op: DeltaOpInsert, Insert: nil},
		{Op: DeltaOpInsert, Insert: "z", Attributes: Attributes{"d": []any{nil}}},
	}
	if got := v1.GetText("t").ToDelta(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("V1 reload\n got=%v\nwant=%v", got, want)
	}
}

// yjsNaNText is Yjs 13.6 (clientID 7) after t.insert(0, 'hello');
// t.format(0, 2, {x: NaN}); t.insertEmbed(5, {v: Infinity}), as V2 and V1.
const (
	yjsNaNTextV2 = "000200020247040302000602000409040084004600c600850e087468656c6c6f78780102034100010100000105007b7ff80000000000007e760101767c7f80000000"
	yjsNaNTextV1 = "0105070004010174026865840701036c6c6f4607000178046e756c6cc6070107020178046e756c6c8507040a7b2276223a6e756c6c7d00"
)

// A Yjs peer's non-finite format and embed values decode, mirror into another
// text via ToDelta or an observer's delta, and re-encode as V1 byte for byte
// like Yjs.
func TestUnit_JSONValue_RemoteNonFiniteMirrors(t *testing.T) {
	u, _ := hex.DecodeString(yjsNaNTextV2)
	src := New(WithClientID(1))
	st := src.GetText("t")
	var deltas [][]Delta
	st.Observe(func(e YTextEvent) { deltas = append(deltas, e.Delta) })
	if err := ApplyUpdateV2(src, u, nil); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint(st.ToDelta())
	if want != fmt.Sprint([]Delta{
		{Op: DeltaOpInsert, Insert: "he", Attributes: Attributes{"x": math.NaN()}},
		{Op: DeltaOpInsert, Insert: "llo"},
		{Op: DeltaOpInsert, Insert: map[string]any{"v": math.Inf(1)}},
	}) {
		t.Fatalf("decoded delta = %s", want)
	}
	if got := hex.EncodeToString(EncodeStateAsUpdateV1(src, nil)); got != yjsNaNTextV1 {
		t.Errorf("V1 re-encode\n got=%s\nwant=%s", got, yjsNaNTextV1)
	}

	copied := New(WithClientID(2))
	ct := copied.GetText("t")
	copied.Transact(func(txn *Transaction) { ct.ApplyDelta(txn, st.ToDelta()) })
	forwarded := New(WithClientID(3))
	ft := forwarded.GetText("t")
	for _, d := range deltas {
		forwarded.Transact(func(txn *Transaction) { ft.ApplyDelta(txn, d) })
	}
	for name, txt := range map[string]*YText{"ToDelta copy": ct, "observer forward": ft} {
		if got := fmt.Sprint(txt.ToDelta()); got != want {
			t.Errorf("%s\n got=%s\nwant=%s", name, got, want)
		}
	}
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
