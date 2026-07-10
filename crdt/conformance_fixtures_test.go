package crdt_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reearth/ygo/crdt"
)

type conformanceFixture struct {
	Name string `json:"name"`
	// Kind partitions mixed-schema fixture files. yxml_yjs_fixtures.json is
	// shared with the y-prosemirror wire-conformance suite
	// (yxml_yjs_conformance_test.go); this file only consumes the
	// "decode_xmlstring" rows (expected = ToXML string, root "x"). The
	// yarray/ytext fixture files carry no kind.
	Kind     string          `json:"kind"`
	V1       string          `json:"v1"`
	V2       string          `json:"v2"`
	Expected json.RawMessage `json:"expected"`
}

func loadConformanceFixtures(t *testing.T, filename string) []conformanceFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", filename))
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	var fx []conformanceFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	if len(fx) == 0 {
		t.Fatalf("%s: no fixtures", filename)
	}
	return fx
}

// normalizeJSON round-trips v through encoding/json so numbers (float64) and
// empty-vs-nil collections compare by value, matching the JS-authored expected.
func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// decodeConformance decodes the fixture's V1 and V2 bytes into fresh docs and
// returns the ToJSON of the named root for each, plus asserts no error/panic.
func decodeConformance(t *testing.T, fx conformanceFixture, toJSON func(*crdt.Doc) ([]byte, error)) (v1json, v2json []byte) {
	t.Helper()
	for _, ver := range []struct {
		tag   string
		hexed string
		apply func(*crdt.Doc, []byte, any) error
		out   *[]byte
	}{
		{"v1", fx.V1, crdt.ApplyUpdateV1, &v1json},
		{"v2", fx.V2, crdt.ApplyUpdateV2, &v2json},
	} {
		raw, err := hex.DecodeString(ver.hexed)
		if err != nil {
			t.Fatalf("%s/%s bad hex: %v", fx.Name, ver.tag, err)
		}
		doc := crdt.New()
		if err := ver.apply(doc, raw, nil); err != nil {
			t.Fatalf("%s/%s decode error: %v", fx.Name, ver.tag, err)
		}
		j, err := toJSON(doc)
		if err != nil {
			t.Fatalf("%s/%s ToJSON: %v", fx.Name, ver.tag, err)
		}
		*ver.out = j
	}
	return v1json, v2json
}

func TestConformance_YArray_DecodeYjsBytes(t *testing.T) {
	for _, fx := range loadConformanceFixtures(t, "yarray_yjs_fixtures.json") {
		fx := fx
		t.Run(fx.Name, func(t *testing.T) {
			v1json, v2json := decodeConformance(t, fx, func(d *crdt.Doc) ([]byte, error) {
				return d.GetArray("a").ToJSON()
			})
			var want any
			if err := json.Unmarshal(fx.Expected, &want); err != nil {
				t.Fatalf("expected: %v", err)
			}
			want = normalizeJSON(t, want)
			for tag, j := range map[string][]byte{"v1": v1json, "v2": v2json} {
				var got any
				if err := json.Unmarshal(j, &got); err != nil {
					t.Fatalf("%s: unmarshal got: %v", tag, err)
				}
				if !reflect.DeepEqual(normalizeJSON(t, got), want) {
					t.Errorf("%s/%s mismatch:\n got=%#v\n want=%#v", fx.Name, tag, got, want)
				}
			}
		})
	}
}

func TestConformance_YText_DecodeYjsBytes(t *testing.T) {
	for _, fx := range loadConformanceFixtures(t, "ytext_yjs_fixtures.json") {
		fx := fx
		t.Run(fx.Name, func(t *testing.T) {
			v1json, v2json := decodeConformance(t, fx, func(d *crdt.Doc) ([]byte, error) {
				return d.GetText("t").ToJSON() // YText.ToJSON returns the JSON-quoted string
			})
			var want string
			if err := json.Unmarshal(fx.Expected, &want); err != nil {
				t.Fatalf("expected: %v", err)
			}
			for tag, j := range map[string][]byte{"v1": v1json, "v2": v2json} {
				var got string
				if err := json.Unmarshal(j, &got); err != nil {
					t.Fatalf("%s: unmarshal got: %v", tag, err)
				}
				if got != want {
					t.Errorf("%s/%s mismatch: got %q want %q", fx.Name, tag, got, want)
				}
			}
		})
	}
}

func TestConformance_YXml_DecodeYjsBytes(t *testing.T) {
	ran := 0
	for _, fx := range loadConformanceFixtures(t, "yxml_yjs_fixtures.json") {
		if fx.Kind != "decode_xmlstring" {
			// The y-prosemirror wire-conformance rows (kind "decode",
			// "author", …) are consumed by yxml_yjs_conformance_test.go.
			continue
		}
		ran++
		fx := fx
		t.Run(fx.Name, func(t *testing.T) {
			for _, ver := range []struct {
				tag   string
				hexed string
				apply func(*crdt.Doc, []byte, any) error
			}{
				{"v1", fx.V1, crdt.ApplyUpdateV1},
				{"v2", fx.V2, crdt.ApplyUpdateV2},
			} {
				raw, err := hex.DecodeString(ver.hexed)
				if err != nil {
					t.Fatalf("%s/%s bad hex: %v", fx.Name, ver.tag, err)
				}
				doc := crdt.New()
				if err := ver.apply(doc, raw, nil); err != nil {
					t.Fatalf("%s/%s decode error: %v", fx.Name, ver.tag, err)
				}
				var want string
				if err := json.Unmarshal(fx.Expected, &want); err != nil {
					t.Fatalf("expected: %v", err)
				}
				if got := doc.GetXmlFragment("x").ToXML(); got != want {
					t.Errorf("%s/%s mismatch: got %q want %q", fx.Name, ver.tag, got, want)
				}
			}
		})
	}
	if ran == 0 {
		t.Fatal("yxml_yjs_fixtures.json: no decode_xmlstring fixtures")
	}
}
