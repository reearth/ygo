package crdt

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// ContentJSON (wire ref 2) is a JSON-text string per value in BOTH V1 and V2
// (Yjs ContentJSON.write: writeLen + writeString(JSON.stringify(c))), not lib0
// Any. Fixtures come from testutil/gen_fixtures_contentjson.js.

type contentJSONFixture struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Exact     bool            `json:"exact"`
	V1        string          `json:"v1"`
	V2        string          `json:"v2"`
	Expected  json.RawMessage `json:"expected"`
	DiffClock *uint64         `json:"diffClock"`
	V1Diff    string          `json:"v1Diff"`
	V2Diff    string          `json:"v2Diff"`
}

const contentJSONClient ClientID = 7

func loadContentJSONFixtures(t *testing.T) []contentJSONFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "contentjson_yjs_fixtures.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var fx []contentJSONFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(fx) == 0 {
		t.Fatal("contentjson_yjs_fixtures.json: no fixtures")
	}
	return fx
}

// contentJSONFuzzSeeds returns the fixtures' full updates for the decode fuzzers.
func contentJSONFuzzSeeds(v2 bool) [][]byte {
	raw, err := os.ReadFile(filepath.Join("testdata", "contentjson_yjs_fixtures.json"))
	if err != nil {
		return nil
	}
	var fx []contentJSONFixture
	if json.Unmarshal(raw, &fx) != nil {
		return nil
	}
	var out [][]byte
	for _, f := range fx {
		h := f.V1
		if v2 {
			h = f.V2
		}
		if b, err := hex.DecodeString(h); err == nil {
			out = append(out, b)
		}
	}
	return out
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// rootJSON returns the fixture root's ToJSON, re-parsed for value comparison.
func rootJSON(t *testing.T, d *Doc, kind string) any {
	t.Helper()
	var j []byte
	var err error
	if kind == "map" {
		j, err = d.GetMap("m").ToJSON()
	} else {
		j, err = d.GetArray("a").ToJSON()
	}
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", j, err)
	}
	return v
}

func countContentJSON(d *Doc) int {
	n := 0
	for _, items := range d.store.clients {
		for _, it := range items {
			if _, ok := it.Content.(*ContentJSON); ok {
				n++
			}
		}
	}
	return n
}

// applyBoth decodes the fixture's V1 and V2 bytes into fresh docs.
func applyBoth(t *testing.T, fx contentJSONFixture) map[string]*Doc {
	t.Helper()
	out := map[string]*Doc{}
	for tag, apply := range map[string]func(*Doc, []byte, any) error{"v1": ApplyUpdateV1, "v2": ApplyUpdateV2} {
		hexed := fx.V1
		if tag == "v2" {
			hexed = fx.V2
		}
		d := New()
		if err := apply(d, mustHex(t, hexed), nil); err != nil {
			t.Fatalf("%s decode: %v", tag, err)
		}
		if ps := d.PendingStats(); ps.Items > 0 {
			t.Fatalf("%s: %d items left pending", tag, ps.Items)
		}
		out[tag] = d
	}
	return out
}

func TestConformance_ContentJSON_DecodeYjsBytes(t *testing.T) {
	for _, fx := range loadContentJSONFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			var want any
			if err := json.Unmarshal(fx.Expected, &want); err != nil {
				t.Fatal(err)
			}
			for tag, d := range applyBoth(t, fx) {
				if countContentJSON(d) == 0 {
					t.Fatalf("%s: no ContentJSON item decoded", tag)
				}
				if got := rootJSON(t, d, fx.Kind); !reflect.DeepEqual(got, want) {
					t.Errorf("%s mismatch:\n got=%#v\nwant=%#v", tag, got, want)
				}
			}
		})
	}
}

// Byte equality with yjs's own encoding is the strongest ygo→Yjs check: every
// re-encode path (full, diff with offset, both converters) must reproduce it.
func TestConformance_ContentJSON_ReencodeMatchesYjs(t *testing.T) {
	for _, fx := range loadContentJSONFixtures(t) {
		if !fx.Exact {
			continue
		}
		t.Run(fx.Name, func(t *testing.T) {
			v1, v2 := mustHex(t, fx.V1), mustHex(t, fx.V2)
			for tag, d := range applyBoth(t, fx) {
				if got := EncodeStateAsUpdateV1(d, nil); !bytes.Equal(got, v1) {
					t.Errorf("%s→V1:\n got=%x\nwant=%x", tag, got, v1)
				}
				if got := EncodeStateAsUpdateV2(d, nil); !bytes.Equal(got, v2) {
					t.Errorf("%s→V2:\n got=%x\nwant=%x", tag, got, v2)
				}
				if fx.DiffClock == nil {
					continue
				}
				sv := StateVector{contentJSONClient: *fx.DiffClock}
				if got, want := EncodeStateAsUpdateV1(d, sv), mustHex(t, fx.V1Diff); !bytes.Equal(got, want) {
					t.Errorf("%s→V1 diff:\n got=%x\nwant=%x", tag, got, want)
				}
				if got, want := EncodeStateAsUpdateV2(d, sv), mustHex(t, fx.V2Diff); !bytes.Equal(got, want) {
					t.Errorf("%s→V2 diff:\n got=%x\nwant=%x", tag, got, want)
				}
			}
			if got, err := UpdateV1ToV2(v1); err != nil || !bytes.Equal(got, v2) {
				t.Errorf("UpdateV1ToV2: err=%v\n got=%x\nwant=%x", err, got, v2)
			}
			if got, err := UpdateV2ToV1(v2); err != nil || !bytes.Equal(got, v1) {
				t.Errorf("UpdateV2ToV1: err=%v\n got=%x\nwant=%x", err, got, v1)
			}
		})
	}
}

// Every byte path ygo can take must preserve the values, including rows whose
// JSON text Go cannot reproduce byte-for-byte (key order, U+2028 escaping).
func TestConformance_ContentJSON_ReencodePreservesValues(t *testing.T) {
	for _, fx := range loadContentJSONFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			var want any
			if err := json.Unmarshal(fx.Expected, &want); err != nil {
				t.Fatal(err)
			}
			v1, v2 := mustHex(t, fx.V1), mustHex(t, fx.V2)
			conv2, err := UpdateV1ToV2(v1)
			if err != nil {
				t.Fatalf("UpdateV1ToV2: %v", err)
			}
			conv1, err := UpdateV2ToV1(v2)
			if err != nil {
				t.Fatalf("UpdateV2ToV1: %v", err)
			}
			merged1, err := MergeUpdatesV1(v1, v1)
			if err != nil {
				t.Fatalf("MergeUpdatesV1: %v", err)
			}
			merged2, err := MergeUpdatesV2(v2, v2)
			if err != nil {
				t.Fatalf("MergeUpdatesV2: %v", err)
			}
			src := applyBoth(t, fx)["v1"]
			cases := []struct {
				name  string
				b     []byte
				apply func(*Doc, []byte, any) error
			}{
				{"encodeV1", EncodeStateAsUpdateV1(src, nil), ApplyUpdateV1},
				{"encodeV2", EncodeStateAsUpdateV2(src, nil), ApplyUpdateV2},
				{"convV1toV2", conv2, ApplyUpdateV2},
				{"convV2toV1", conv1, ApplyUpdateV1},
				{"mergeV1", merged1, ApplyUpdateV1},
				{"mergeV2", merged2, ApplyUpdateV2},
			}
			for _, c := range cases {
				d := New()
				if err := c.apply(d, c.b, nil); err != nil {
					t.Fatalf("%s apply: %v", c.name, err)
				}
				if got := rootJSON(t, d, fx.Kind); !reflect.DeepEqual(got, want) {
					t.Errorf("%s mismatch:\n got=%#v\nwant=%#v", c.name, got, want)
				}
			}
		})
	}
}

// Go-authored values round-trip identically through V1 and V2.
func TestUnit_ContentJSON_GoValuesSymmetricV1V2(t *testing.T) {
	vals := []any{int64(42), 1.25, "s<&>", nil, true, []any{int64(1), "x"}, map[string]any{"b": int64(2), "a": nil}}
	want := []any{float64(42), 1.25, "s<&>", nil, true, []any{float64(1), "x"}, map[string]any{"b": float64(2), "a": nil}}

	v1 := encoding.NewEncoder()
	encodeContent(v1, NewContentJSON(vals...), 0)
	got1, err := decodeContent(encoding.NewDecoder(v1.Bytes()), nil, wireJSON)
	if err != nil {
		t.Fatalf("V1 decode: %v", err)
	}
	if !reflect.DeepEqual(got1.(*ContentJSON).Vals, want) {
		t.Errorf("V1:\n got=%#v\nwant=%#v", got1.(*ContentJSON).Vals, want)
	}

	d := New()
	arr := d.GetArray("a")
	d.Transact(func(txn *Transaction) {
		item := &Item{
			ID:      ID{Client: d.clientID, Clock: d.store.NextClock(d.clientID)},
			Parent:  arr.baseType(),
			Content: NewContentJSON(vals...),
		}
		item.integrate(txn, 0)
	})
	for tag, b := range map[string][]byte{"v1": EncodeStateAsUpdateV1(d, nil), "v2": EncodeStateAsUpdateV2(d, nil)} {
		d2 := New()
		apply := ApplyUpdateV1
		if tag == "v2" {
			apply = ApplyUpdateV2
		}
		if err := apply(d2, b, nil); err != nil {
			t.Fatalf("%s apply: %v", tag, err)
		}
		if got := d2.GetArray("a").ToSlice(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got=%#v\nwant=%#v", tag, got, want)
		}
	}
}

// ContentJSON payloads that are not JSON text or not UTF-8 fail the apply in
// both versions. Each replaces the fixture's first 9-byte "undefined" in place.
func TestUnit_ContentJSON_RejectsMalformed(t *testing.T) {
	var fx contentJSONFixture
	for _, f := range loadContentJSONFixtures(t) {
		if f.Name == "array_undefined_only" {
			fx = f
		}
	}
	for name, repl := range map[string][]byte{
		"bad_json": []byte("{nopenope"),
		"bad_utf8": {'"', 0xff, 'a', 'a', 'a', 'a', 'a', 'a', '"'},
	} {
		for tag, c := range map[string]struct {
			hexed string
			apply func(*Doc, []byte, any) error
		}{"v1": {fx.V1, ApplyUpdateV1}, "v2": {fx.V2, ApplyUpdateV2}} {
			t.Run(name+"/"+tag, func(t *testing.T) {
				b := mustHex(t, c.hexed)
				mut := bytes.Replace(b, []byte("undefined"), repl, 1)
				if bytes.Equal(mut, b) {
					t.Fatal("fixture has no 'undefined' to replace")
				}
				d := New()
				if err := c.apply(d, mut, nil); !errors.Is(err, ErrInvalidUpdate) {
					t.Fatalf("err = %v, want ErrInvalidUpdate", err)
				}
				if d.GetArray("a").Len() != 0 {
					t.Fatal("malformed update partially integrated")
				}
			})
		}
	}
}

// Every read accessor sees ContentJSON values, not just ToSlice/Entries.
func TestUnit_ContentJSON_ReadAccessors(t *testing.T) {
	for _, fx := range loadContentJSONFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			d := applyBoth(t, fx)["v1"]
			if fx.Kind == "map" {
				m := d.GetMap("m")
				want := m.Entries()
				got := map[string]any{}
				m.ForEach(func(k string, v any) { got[k] = v })
				if !reflect.DeepEqual(got, want) {
					t.Errorf("ForEach:\n got=%#v\nwant=%#v", got, want)
				}
				for k, w := range want {
					if v, ok := m.Get(k); !ok || !reflect.DeepEqual(v, w) {
						t.Errorf("Get(%q) = %#v, %v; want %#v", k, v, ok, w)
					}
				}
				return
			}
			a := d.GetArray("a")
			want := a.ToSlice()
			if got := a.Slice(0, a.Len()); !reflect.DeepEqual(got, want) {
				t.Errorf("Slice:\n got=%#v\nwant=%#v", got, want)
			}
			var each []any
			a.ForEach(func(_ int, v any) { each = append(each, v) })
			if !reflect.DeepEqual(each, want) {
				t.Errorf("ForEach:\n got=%#v\nwant=%#v", each, want)
			}
			for i, w := range want {
				if v := a.Get(i); !reflect.DeepEqual(v, w) {
					t.Errorf("Get(%d) = %#v, want %#v", i, v, w)
				}
			}
		})
	}
}
