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
	Name     string                  `json:"name"`
	V1       strandedWire            `json:"v1"`
	V2       strandedWire            `json:"v2"`
	Expected map[string]strandedSets `json:"expected"`
}

type strandedSets struct {
	Live    [][]int `json:"live"`
	Deleted [][]int `json:"deleted"`
}

type strandedWire struct {
	Base    string   `json:"base"`
	Updates []string `json:"updates"`
}

// strandedSetsOf reports the doc's live insert set and delete set in the
// fixture's [client, clock, length] shape, sorted by client then clock.
func strandedSetsOf(doc *crdt.Doc) strandedSets {
	return strandedSets{
		Live:    idRanges(crdt.InsertSetFromDoc(doc, true)),
		Deleted: idRanges(crdt.DeleteSetFromDoc(doc)),
	}
}

func idRanges(set *crdt.IDSet) [][]int {
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
// ygo leaves the same items live and deleted as yjs does.
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
				applyAll(t, ver.apply, single, base)
				for _, u := range updates {
					applyAll(t, ver.apply, single, u)
				}
				assertNothingPending(t, single)
				if got := strandedSetsOf(single); !reflect.DeepEqual(got, fx.Expected["singleDoc"]) {
					t.Errorf("%s single doc: got %+v, yjs %+v", ver.tag, got, fx.Expected["singleDoc"])
				}

				state := base
				var persisted *crdt.Doc
				for _, u := range updates {
					persisted = crdt.New()
					applyAll(t, ver.apply, persisted, state)
					applyAll(t, ver.apply, persisted, u)
					state = ver.encode(persisted, nil)
				}
				assertNothingPending(t, persisted)
				if got := strandedSetsOf(persisted); !reflect.DeepEqual(got, fx.Expected["persisting"]) {
					t.Errorf("%s persist/reload: got %+v, yjs %+v", ver.tag, got, fx.Expected["persisting"])
				}
			}
		})
	}
}

// mustApply applies update and requires it to integrate fully.
func mustApply(t *testing.T, apply func(*crdt.Doc, []byte, any) error, doc *crdt.Doc, update []byte) {
	t.Helper()
	applyAll(t, apply, doc, update)
	assertNothingPending(t, doc)
}

// applyAll applies update, which may park items until a later update arrives.
func applyAll(t *testing.T, apply func(*crdt.Doc, []byte, any) error, doc *crdt.Doc, update []byte) {
	t.Helper()
	if err := apply(doc, update, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func assertNothingPending(t *testing.T, doc *crdt.Doc) {
	t.Helper()
	if ps := doc.PendingStats(); ps.Items > 0 {
		t.Fatalf("%d items left pending", ps.Items)
	}
}
