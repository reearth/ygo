package crdt

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type dupGroupsFixture struct {
	Name     string `json:"name"`
	V1       string `json:"v1"`
	PrefixV1 string `json:"prefixV1"`
	V2       string `json:"v2"`
	PrefixV2 string `json:"prefixV2"`
	Expected struct {
		Text     string                 `json:"text"`
		Other    string                 `json:"other"`
		Map      any                    `json:"map"`
		SV       map[string]uint64      `json:"sv"`
		Coverage map[string][][2]uint64 `json:"coverage"`
		Pending  bool                   `json:"pending"`
	} `json:"expected"`
}

// yjs keeps only the last wire group per client and drops earlier ones
// unintegrated; ygo must reach the same content, state vector and struct
// coverage.
func TestConformance_RepeatedClientGroups_MatchYjs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "dupgroups_yjs_fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []dupGroupsFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	for _, fx := range fixtures {
		for _, ver := range []struct {
			tag            string
			update, prefix string
			apply          func(*Doc, []byte, any) error
			merge          func(...[]byte) ([]byte, error)
			diff           func([]byte, StateVector) ([]byte, error)
		}{
			{"v1", fx.V1, fx.PrefixV1, ApplyUpdateV1, MergeUpdatesV1, DiffUpdateV1},
			{"v2", fx.V2, fx.PrefixV2, ApplyUpdateV2, MergeUpdatesV2, DiffUpdateV2},
		} {
			if ver.update == "" {
				continue
			}
			// Merging or diffing the update must not change what it applies.
			routes := map[string]func([]byte) ([]byte, error){
				"apply": func(b []byte) ([]byte, error) { return b, nil },
				"merge": func(b []byte) ([]byte, error) { return ver.merge(b) },
				"diff":  func(b []byte) ([]byte, error) { return ver.diff(b, StateVector{}) },
			}
			for _, route := range []string{"apply", "merge", "diff"} {
				t.Run(fx.Name+"/"+ver.tag+"/"+route, func(t *testing.T) {
					doc := New()
					defer doc.Destroy()
					for i, h := range []string{ver.prefix, ver.update} {
						if h == "" {
							continue
						}
						b, err := hex.DecodeString(h)
						if err != nil {
							t.Fatal(err)
						}
						if i == 1 {
							if b, err = routes[route](b); err != nil {
								t.Fatalf("%s: %v", route, err)
							}
						}
						if err := ver.apply(doc, b, nil); err != nil {
							t.Fatalf("apply: %v", err)
						}
					}
					want := fx.Expected
					if got := doc.GetText("t").ToString(); got != want.Text {
						t.Errorf("text = %q, want %q", got, want.Text)
					}
					if got := doc.GetText("u").ToString(); got != want.Other {
						t.Errorf("other = %q, want %q", got, want.Other)
					}
					raw, err := doc.GetMap("m").ToJSON()
					if err != nil {
						t.Fatal(err)
					}
					var gotMap any
					if err := json.Unmarshal(raw, &gotMap); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(gotMap, want.Map) {
						t.Errorf("map = %v, want %v", gotMap, want.Map)
					}
					sv := map[string]uint64{}
					for c, clock := range doc.StateVector() {
						sv[fmt.Sprint(c)] = clock
					}
					if !reflect.DeepEqual(sv, want.SV) {
						t.Errorf("state vector = %v, want %v", sv, want.SV)
					}
					coverage := map[string][][2]uint64{}
					for c, items := range doc.store.clients {
						var ranges [][2]uint64
						for _, it := range items {
							end := it.ID.Clock + uint64(it.Content.Len())
							if n := len(ranges); n > 0 && ranges[n-1][1] == it.ID.Clock {
								ranges[n-1][1] = end
							} else {
								ranges = append(ranges, [2]uint64{it.ID.Clock, end})
							}
						}
						if len(ranges) > 0 {
							coverage[fmt.Sprint(c)] = ranges
						}
					}
					if !reflect.DeepEqual(coverage, want.Coverage) {
						t.Errorf("store coverage = %v, want %v", coverage, want.Coverage)
					}
					if got := doc.PendingStats().Items > 0; got != want.Pending {
						t.Errorf("pending = %v, want %v", got, want.Pending)
					}
				})
			}
		}
	}
}
