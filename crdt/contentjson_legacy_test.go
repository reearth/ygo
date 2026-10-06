package crdt

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// ygo ≤1.51.0 wrote V1 ContentJSON as lib0 Any per value. It could only get a
// ContentJSON by decoding Yjs V2, so persisted V1 bytes in that form exist.
// legacyContentJSONV1 is 54a8fd4's encodeContent for wireJSON.
func legacyContentJSONV1(vals ...any) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(uint64(len(vals)))
	for _, v := range vals {
		enc.WriteAny(v)
	}
	return enc.Bytes()
}

// 54a8fd4's EncodeStateAsUpdateV1 (and UpdateV2ToV1, byte-identical) of each
// fixture's Yjs V2 update.
var legacyContentJSONV1Fixtures = map[string]string{
	"array_scalars":           "01010700020101610d7c3f8000007cc04000007c3fc000007b444b1ae4d6e2ef507b3e7ad7f29abcaf487b4340000000000001770374776f7700770b68c3a96c6c6f20f09f988077053c263e225c78797e00",
	"array_nested":            "0101070002010161047601016b75047c3f8000007e7876010178770179750076007502750075027c3f800000750177046465657000",
	"array_undefined_only":    "0101070002010161027e7e00",
	"array_between_any_items": "01030700080101610177066265666f72658207000377026a307c400000007e88070302770561667465727d0300",
	"array_split_by_diff":     "010107000201016106770273307c3f8000007601016b7701767e75017c4000000077047461696c00",
	"map_value":               "010207002201016d046a736f6e017601066e657374656475027c3f800000770374776f2801016d03616e79017705706c61696e00",
	"map_value_undefined":     "010107002201016d05756e646566017e00",
	"array_semantic_only":     "010107000201016103760301617c40000000016d760201627c4000000001797c3f800000017a7c3f800000770a6c696e65e280a87365707c0000000000",
}

func TestUnit_ContentJSON_DecodesLegacyAnyV1(t *testing.T) {
	fixtures := loadContentJSONFixtures(t)
	if len(fixtures) != len(legacyContentJSONV1Fixtures) {
		t.Fatalf("%d fixtures, %d legacy encodings", len(fixtures), len(legacyContentJSONV1Fixtures))
	}
	for _, fx := range fixtures {
		t.Run(fx.Name, func(t *testing.T) {
			legacy := mustHex(t, legacyContentJSONV1Fixtures[fx.Name])
			var want any
			if err := json.Unmarshal(fx.Expected, &want); err != nil {
				t.Fatal(err)
			}

			// The helper reproduces 54a8fd4's bytes for every ContentJSON item.
			src := applyBoth(t, fx)["v2"]
			for _, items := range src.store.clients {
				for _, it := range items {
					if c, ok := it.Content.(*ContentJSON); ok && !bytes.Contains(legacy, legacyContentJSONV1(c.Vals...)) {
						t.Fatalf("helper bytes %x not in legacy update", legacyContentJSONV1(c.Vals...))
					}
				}
			}

			d := New()
			if err := ApplyUpdateV1(d, legacy, nil); err != nil {
				t.Fatalf("apply legacy: %v", err)
			}
			if got := rootJSON(t, d, fx.Kind); !reflect.DeepEqual(got, want) {
				t.Errorf("legacy mismatch:\n got=%#v\nwant=%#v", got, want)
			}
			// Values match the Yjs-decoded doc's, Go types included.
			if got, wantVals := plainJSONVals(d), plainJSONVals(src); !reflect.DeepEqual(got, wantVals) {
				t.Errorf("legacy vals:\n got=%#v\nwant=%#v", got, wantVals)
			}

			// Re-encoding heals to the Yjs form.
			if fx.Exact {
				if got, wantB := EncodeStateAsUpdateV1(d, nil), mustHex(t, fx.V1); !bytes.Equal(got, wantB) {
					t.Errorf("re-encode:\n got=%x\nwant=%x", got, wantB)
				}
			}
			healed := New()
			if err := ApplyUpdateV1(healed, EncodeStateAsUpdateV1(d, nil), nil); err != nil {
				t.Fatalf("apply healed: %v", err)
			}
			if got := rootJSON(t, healed, fx.Kind); !reflect.DeepEqual(got, want) {
				t.Errorf("healed mismatch:\n got=%#v\nwant=%#v", got, want)
			}
			if conv, err := UpdateV1ToV2(legacy); err != nil {
				t.Errorf("UpdateV1ToV2(legacy): %v", err)
			} else if fx.Exact && !bytes.Equal(conv, mustHex(t, fx.V2)) {
				t.Errorf("UpdateV1ToV2(legacy):\n got=%x\nwant=%s", conv, fx.V2)
			}
		})
	}
}

func plainJSONVals(d *Doc) [][]any {
	var out [][]any
	for _, items := range d.store.clients {
		for _, it := range items {
			if c, ok := it.Content.(*ContentJSON); ok {
				out = append(out, c.Vals)
			}
		}
	}
	return out
}

// jsonOfLen returns a JSON string literal of exactly n bytes.
func jsonOfLen(n int) string { return `"` + strings.Repeat("a", n-2) + `"` }

// A JSON text of 116–127 bytes has a length byte equal to an Any tag; both
// encoders' output of such values must decode to what was written.
func TestUnit_ContentJSON_AmbiguousLengthBothForms(t *testing.T) {
	for n := 116; n <= 127; n++ {
		val := strings.Repeat("a", n-2)
		yjs := encoding.NewEncoder()
		encodeContent(yjs, NewContentJSON(val, val), 0)
		if b := yjs.Bytes(); b[1] != byte(n) {
			t.Fatalf("n=%d: length byte %d", n, b[1])
		}
		for name, b := range map[string][]byte{"yjs": yjs.Bytes(), "legacy": legacyContentJSONV1(val, val)} {
			got, err := decodeContent(encoding.NewDecoder(b), nil, wireJSON)
			if err != nil {
				t.Fatalf("n=%d %s: %v", n, name, err)
			}
			if want := []any{val, val}; !reflect.DeepEqual(got.(*ContentJSON).Vals, want) {
				t.Errorf("n=%d %s: got %#v", n, name, got.(*ContentJSON).Vals)
			}
		}
	}

	// Through a whole update, so the decoder lands on the next struct.
	for n := 116; n <= 127; n++ {
		val := strings.Repeat("b", n-2)
		d := New()
		arr := d.GetArray("a")
		d.Transact(func(txn *Transaction) {
			item := &Item{
				ID:      ID{Client: d.clientID, Clock: d.store.NextClock(d.clientID)},
				Parent:  arr.baseType(),
				Content: NewContentJSON(val, val),
			}
			item.integrate(txn, 0)
			arr.Push(txn, []any{"after"})
		})
		d2 := New()
		if err := ApplyUpdateV1(d2, EncodeStateAsUpdateV1(d, nil), nil); err != nil {
			t.Fatalf("n=%d apply: %v", n, err)
		}
		if got, want := d2.GetArray("a").ToSlice(), []any{val, val, "after"}; !reflect.DeepEqual(got, want) {
			t.Errorf("n=%d update: got %#v", n, got)
		}
	}
}

// Every Any tag decodes in the legacy form, numbers as JSON's float64.
func TestUnit_ContentJSON_LegacyEveryAnyTag(t *testing.T) {
	for _, c := range []struct {
		val  any
		tag  byte
		want any
	}{
		{[]byte{9}, 116, []byte{9}},
		{[]any{1.5, int64(2)}, 117, []any{1.5, float64(2)}},
		{map[string]any{"k": float32(0.5)}, 118, map[string]any{"k": 0.5}},
		{"s", 119, "s"},
		{true, 120, true},
		{false, 121, false},
		{encoding.BigInt(7), 122, encoding.BigInt(7)},
		{0.1, 123, 0.1},
		{1.5, 124, 1.5},
		{int64(-5), 125, float64(-5)},
		{nil, 126, nil},
	} {
		b := legacyContentJSONV1(c.val, "tail")
		if b[1] != c.tag {
			t.Fatalf("%#v: tag %d, want %d", c.val, b[1], c.tag)
		}
		got, err := decodeContent(encoding.NewDecoder(b), nil, wireJSON)
		if err != nil {
			t.Fatalf("tag %d: %v", c.tag, err)
		}
		if want := []any{c.want, "tail"}; !reflect.DeepEqual(got.(*ContentJSON).Vals, want) {
			t.Errorf("tag %d:\n got=%#v\nwant=%#v", c.tag, got.(*ContentJSON).Vals, want)
		}
	}
	// undefined (127) has no Go value to encode from.
	got, err := decodeContent(encoding.NewDecoder([]byte{1, 127}), nil, wireJSON)
	if err != nil || !reflect.DeepEqual(got.(*ContentJSON).Vals, []any{nil}) {
		t.Errorf("tag 127: %v, %v", got, err)
	}
}

// Bytes valid under both readings decode as JSON text: Yjs wins ties.
func TestUnit_ContentJSON_TiePrefersJSONText(t *testing.T) {
	// Any reading: varint 34 (tag 125, byte '"'). JSON reading: a 125-byte
	// string literal starting at that '"'.
	b := append([]byte{1, 125}, jsonOfLen(125)...)
	if _, err := encoding.NewDecoder(b[1:]).ReadAny(); err != nil {
		t.Fatalf("not a valid Any: %v", err)
	}
	got, err := decodeContent(encoding.NewDecoder(b), nil, wireJSON)
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{strings.Repeat("a", 123)}; !reflect.DeepEqual(got.(*ContentJSON).Vals, want) {
		t.Errorf("got %#v, want JSON-text reading", got.(*ContentJSON).Vals)
	}
}

// An item is in one form: a JSON-text first value with an Any second is garbage.
func TestUnit_ContentJSON_LegacyRejectsMixedAndMalformed(t *testing.T) {
	jsonFirst := append([]byte{2, 4}, `"ab"`...)
	jsonFirst = append(jsonFirst, 119, 1, 'x')
	ambiguousFirst := append([]byte{2, 119}, jsonOfLen(119)...)
	for name, b := range map[string][]byte{
		"json_then_any":   jsonFirst,
		"any_then_json":   append([]byte{2, 126, 4}, `"ab"`...),
		"neither":         {1, 0xff},
		"bad_any_tag":     {1, 125},
		"truncated_any":   {1, 119, 5, 'a'},
		"truncated_ambig": ambiguousFirst[:50],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeContent(encoding.NewDecoder(b), nil, wireJSON); err == nil {
				t.Fatalf("%x decoded", b)
			}
		})
	}

	// Through ApplyUpdateV1: nothing integrated.
	legacy := mustHex(t, legacyContentJSONV1Fixtures["array_undefined_only"])
	mut := bytes.Replace(legacy, []byte{0x02, 0x7e, 0x7e}, []byte{0x02, 0x7e, 0x73}, 1)
	if bytes.Equal(mut, legacy) {
		t.Fatal("no replacement")
	}
	d := New()
	if err := ApplyUpdateV1(d, mut, nil); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("err = %v, want ErrInvalidUpdate", err)
	}
	if d.GetArray("a").Len() != 0 {
		t.Fatal("malformed legacy update partially integrated")
	}
}
