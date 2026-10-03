package crdt_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reearth/ygo/crdt"
)

type strandedFixture struct {
	Name     string             `json:"name"`
	V1       strandedWire       `json:"v1"`
	V2       strandedWire       `json:"v2"`
	Expected map[string][][]int `json:"expected"`
}

type strandedWire struct {
	Base    string   `json:"base"`
	Updates []string `json:"updates"`
}

// liveRanges reports the doc's live insert set in the fixture's
// [client, clock, length] shape, sorted by client then clock.
func liveRanges(doc *crdt.Doc) [][]int {
	set := crdt.InsertSetFromDoc(doc, true)
	out := [][]int{}
	for _, client := range set.Clients() {
		for _, r := range set.Ranges(client) {
			out = append(out, []int{int(client), int(r.Clock), int(r.Len)})
		}
	}
	return out
}

// TestConformance_StrandedItems_MatchYjsLiveness replays yjs-authored updates in
// which one peer writes into a nested type another peer deleted, and asserts
// ygo leaves the same items live as yjs does.
//
// yjs never keeps such a write: Item.integrate deletes an item whose parent is
// deleted, and an item whose parent was already collected integrates as a GC
// struct. Visible content agrees either way, so the cross-impl fuzz oracle
// (which compares ToJSON/ToXML) cannot see the difference; it shows up in the
// insert set, the delete set and every later encoding of the document.
func TestConformance_StrandedItems_MatchYjsLiveness(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stranded_yjs_fixtures.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixtures []strandedFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	for _, fx := range fixtures {
		t.Run(fx.Name, func(t *testing.T) {
			for _, ver := range []struct {
				tag    string
				wire   strandedWire
				apply  func(*crdt.Doc, []byte, any) error
				encode func(*crdt.Doc, crdt.StateVector) []byte
			}{
				{"v1", fx.V1, crdt.ApplyUpdateV1, crdt.EncodeStateAsUpdateV1},
				{"v2", fx.V2, crdt.ApplyUpdateV2, crdt.EncodeStateAsUpdateV2},
			} {
				base := mustHex(t, ver.wire.Base)
				updates := make([][]byte, len(ver.wire.Updates))
				for i, u := range ver.wire.Updates {
					updates[i] = mustHex(t, u)
				}

				single := crdt.New()
				mustApply(t, ver.apply, single, base)
				for _, u := range updates {
					mustApply(t, ver.apply, single, u)
				}
				if got := liveRanges(single); !reflect.DeepEqual(got, fx.Expected["singleDoc"]) {
					t.Errorf("%s single doc: live %v, yjs %v", ver.tag, got, fx.Expected["singleDoc"])
				}

				state := base
				var persisted *crdt.Doc
				for _, u := range updates {
					persisted = crdt.New()
					mustApply(t, ver.apply, persisted, state)
					mustApply(t, ver.apply, persisted, u)
					state = ver.encode(persisted, nil)
				}
				if got := liveRanges(persisted); !reflect.DeepEqual(got, fx.Expected["persisting"]) {
					t.Errorf("%s persist/reload: live %v, yjs %v", ver.tag, got, fx.Expected["persisting"])
				}
			}
		})
	}
}

func mustApply(t *testing.T, apply func(*crdt.Doc, []byte, any) error, doc *crdt.Doc, update []byte) {
	t.Helper()
	if err := apply(doc, update, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ps := doc.PendingStats(); ps.Items > 0 {
		t.Fatalf("%d items left pending", ps.Items)
	}
}
