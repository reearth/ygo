package crdt

import (
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// deltaAsYjsJSON renders an insert-only delta in Yjs's toDelta JSON shape.
func deltaAsYjsJSON(t *testing.T, d []Delta) any {
	t.Helper()
	ops := make([]map[string]any, 0, len(d))
	for _, op := range d {
		m := map[string]any{"insert": op.Insert}
		if len(op.Attributes) > 0 {
			m["attributes"] = op.Attributes
		}
		ops = append(ops, m)
	}
	b, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Yjs's applyDelta inserts non-string values as embeds; ygo's must emit the
// same V1 bytes for a ToDelta copy and for forwarded observer deltas.
func TestConformance_ApplyDelta_Embeds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "applydelta_yjs_fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fxs []struct {
		Name      string          `json:"name"`
		V1        string          `json:"v1"`
		Delta     json.RawMessage `json:"delta"`
		CopyV1    string          `json:"copyV1"`
		ForwardV1 *string         `json:"forwardV1"`
	}
	if err := json.Unmarshal(raw, &fxs); err != nil {
		t.Fatal(err)
	}
	if len(fxs) == 0 {
		t.Fatal("no fixtures")
	}
	for _, fx := range fxs {
		t.Run(fx.Name, func(t *testing.T) {
			var want any
			if err := json.Unmarshal(fx.Delta, &want); err != nil {
				t.Fatal(err)
			}
			update, _ := hex.DecodeString(fx.V1)
			src := New(WithClientID(1))
			st := src.GetText("t")
			var deltas [][]Delta
			st.Observe(func(e YTextEvent) { deltas = append(deltas, e.Delta) })
			if err := ApplyUpdateV1(src, update, nil); err != nil {
				t.Fatal(err)
			}
			if got := deltaAsYjsJSON(t, st.ToDelta()); !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded %v, want %v", got, want)
			}

			copied := New(WithClientID(9))
			ct := copied.GetText("t")
			copied.Transact(func(txn *Transaction) { ct.ApplyDelta(txn, st.ToDelta()) })
			if got := hex.EncodeToString(EncodeStateAsUpdateV1(copied, nil)); got != fx.CopyV1 {
				t.Errorf("ToDelta copy V1\n got=%s\nwant=%s", got, fx.CopyV1)
			}

			fwd := New(WithClientID(9))
			ft := fwd.GetText("t")
			for _, d := range deltas {
				fwd.Transact(func(txn *Transaction) { ft.ApplyDelta(txn, d) })
			}
			if got := deltaAsYjsJSON(t, ft.ToDelta()); !reflect.DeepEqual(got, want) {
				t.Errorf("forwarded %v, want %v", got, want)
			}
			if fx.ForwardV1 != nil {
				if got := hex.EncodeToString(EncodeStateAsUpdateV1(fwd, nil)); got != *fx.ForwardV1 {
					t.Errorf("forwarded V1\n got=%s\nwant=%s", got, *fx.ForwardV1)
				}
			}
		})
	}
}

// Embeds inserted mid-text land at their delta index with their attributes,
// and survive V1 and V2 reloads.
func TestUnit_ApplyDelta_EmbedsAtIndex(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcd", nil) })
	doc.Transact(func(txn *Transaction) {
		txt.ApplyDelta(txn, []Delta{
			{Op: DeltaOpRetain, Retain: 1},
			{Op: DeltaOpInsert, Insert: map[string]any{"img": "x"}, Attributes: Attributes{"w": int64(2)}},
			{Op: DeltaOpRetain, Retain: 2},
			{Op: DeltaOpInsert, Insert: []any{"y"}},
			{Op: DeltaOpInsert}, // nil inserts nothing; Yjs cannot embed null
			{Op: DeltaOpInsert, Insert: "Z", Attributes: Attributes{"bold": true}},
		})
	})
	want := []Delta{
		{Op: DeltaOpInsert, Insert: "a"},
		{Op: DeltaOpInsert, Insert: map[string]any{"img": "x"}, Attributes: Attributes{"w": int64(2)}},
		{Op: DeltaOpInsert, Insert: "bc"},
		{Op: DeltaOpInsert, Insert: []any{"y"}},
		{Op: DeltaOpInsert, Insert: "Z", Attributes: Attributes{"bold": true}},
		{Op: DeltaOpInsert, Insert: "d"},
	}
	wantJSON := deltaAsYjsJSON(t, want)
	if got := deltaAsYjsJSON(t, txt.ToDelta()); !reflect.DeepEqual(got, wantJSON) {
		t.Fatalf("delta %v, want %v", got, wantJSON)
	}
	if txt.Len() != 7 {
		t.Fatalf("Len = %d, want 7", txt.Len())
	}
	for name, rt := range map[string]func() *Doc{
		"V1": func() *Doc { d := New(); _ = ApplyUpdateV1(d, EncodeStateAsUpdateV1(doc, nil), nil); return d },
		"V2": func() *Doc { d := New(); _ = ApplyUpdateV2(d, EncodeStateAsUpdateV2(doc, nil), nil); return d },
	} {
		if got := deltaAsYjsJSON(t, rt().GetText("t").ToDelta()); !reflect.DeepEqual(got, wantJSON) {
			t.Errorf("%s reload %v, want %v", name, got, wantJSON)
		}
	}
}

// Random deltas mixing embeds, text, retains and deletes, interleaved with
// positional edits, must match a plain slice model.
func TestUnit_ApplyDelta_EmbedsKeepPositions(t *testing.T) {
	for seed := uint64(0); seed < 200; seed++ {
		r := rand.New(rand.NewPCG(seed, 0xe3b))
		doc := newTestDoc(1)
		txt := doc.GetText("t")
		var model []any
		next := 0
		val := func() any {
			next++
			if r.IntN(2) == 0 {
				return string(rune('a' + next%26))
			}
			return map[string]any{"e": int64(next)}
		}
		for step := 0; step < 30; step++ {
			if r.IntN(3) == 0 {
				i := r.IntN(len(model) + 1)
				c := string(rune('A' + next%26))
				next++
				doc.Transact(func(txn *Transaction) { txt.Insert(txn, i, c, Attributes{"b": next%2 == 0}) })
				model = append(model[:i:i], append([]any{c}, model[i:]...)...)
			} else {
				var delta []Delta
				var out []any
				at := 0
				for k := r.IntN(4) + 1; k > 0; k-- {
					switch rest := len(model) - at; {
					case r.IntN(3) == 0 && rest > 0:
						n := r.IntN(rest) + 1
						delta = append(delta, Delta{Op: DeltaOpRetain, Retain: n})
						out = append(out, model[at:at+n]...)
						at += n
					case r.IntN(3) == 0 && rest > 0:
						n := r.IntN(min(rest, 3)) + 1
						delta = append(delta, Delta{Op: DeltaOpDelete, Delete: n})
						at += n
					default:
						v := val()
						delta = append(delta, Delta{Op: DeltaOpInsert, Insert: v})
						out = append(out, v)
					}
				}
				doc.Transact(func(txn *Transaction) { txt.ApplyDelta(txn, delta) })
				model = append(out, model[at:]...)
			}
			var got []any
			for _, d := range txt.ToDelta() {
				if s, ok := d.Insert.(string); ok {
					for _, c := range s {
						got = append(got, string(c))
					}
				} else {
					got = append(got, d.Insert)
				}
			}
			if !reflect.DeepEqual(got, model) {
				t.Fatalf("seed %d step %d:\n got=%v\nwant=%v", seed, step, got, model)
			}
		}
	}
}

// A shared type cannot be embedded in a YText: InsertEmbed and ApplyDelta
// reject it rather than storing a handle V1 writes as {} and V2 cannot write.
func TestUnit_TextEmbed_RejectsSharedTypes(t *testing.T) {
	for name, v := range map[string]any{
		"prelim map":    NewMapPrelim(),
		"prelim text":   NewTextPrelim(),
		"attached text": New().GetText("x"),
		"nested":        map[string]any{"k": []any{NewArrayPrelim()}},
	} {
		t.Run(name, func(t *testing.T) {
			doc := newTestDoc(1)
			txt := doc.GetText("t")
			before := EncodeStateAsUpdateV1(doc, nil)
			doc.Transact(func(txn *Transaction) {
				mustPanicContaining(t, "shared type", func() { txt.InsertEmbed(txn, 0, v, nil) })
				mustPanicContaining(t, "shared type", func() {
					txt.ApplyDelta(txn, []Delta{{Op: DeltaOpInsert, Insert: "ok"}, {Op: DeltaOpInsert, Insert: v}})
				})
				mustPanicContaining(t, "shared type", func() { txt.Insert(txn, 0, "a", Attributes{"k": v}) })
			})
			if got := EncodeStateAsUpdateV1(doc, nil); !reflect.DeepEqual(got, before) {
				t.Fatal("rejected call wrote to the document")
			}
		})
	}
}
