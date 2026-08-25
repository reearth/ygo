package crdt_test

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// TestConformance_GCStructs_DecodeYjsBytes decodes yjs-authored updates that
// contain GC STRUCTS and asserts ygo reads the same document yjs does, over both
// wire versions.
//
// This is the coverage the other fixture files cannot provide. They are built
// from single-client documents with no deletion history, so not one of their 202
// V2 fixtures carries a GC struct — yjs only emits those once deleted content
// has actually been collected, which needs a deleted nested shared type and a
// transaction boundary. That gap is how #231 shipped: ApplyUpdateV2 recorded the
// GC range in the delete set but never advanced the client's struct list, so
// every later struct from that client looked like a clock gap and was parked in
// the pending queue permanently. It returned nil while doing it.
//
// The V1 path has always handled this correctly, which is why every row here
// runs BOTH versions: the V1 column is the control that proves the fixture and
// the expectation are right, and any V2-only failure is a decoder asymmetry.
func TestConformance_GCStructs_DecodeYjsBytes(t *testing.T) {
	fixtures := loadConformanceFixtures(t, "gc_yjs_fixtures.json")
	for _, fx := range fixtures {
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

				// A full-state update is self-contained: everything it
				// references is inside it. Anything left in the pending queue
				// means the decoder could not integrate part of the document,
				// which is the silent half of #231 and worth failing on
				// directly rather than only via the content comparison.
				if ps := doc.PendingStats(); ps.Items > 0 {
					t.Errorf("%s/%s: %d items left unintegrated after a full-state apply (awaiting %d clients); the decode is incomplete",
						fx.Name, ver.tag, ps.Items, len(ps.MissingFor))
				}

				switch fx.Kind {
				case "xmlfrag":
					var want string
					if err := json.Unmarshal(fx.Expected, &want); err != nil {
						t.Fatalf("expected: %v", err)
					}
					if got := doc.GetXmlFragment("x").ToXML(); got != want {
						t.Errorf("%s/%s mismatch:\n got=%q\nwant=%q", fx.Name, ver.tag, got, want)
					}
				default:
					var j []byte
					var err error
					if fx.Kind == "array" {
						j, err = doc.GetArray("a").ToJSON()
					} else {
						j, err = doc.GetMap("m").ToJSON()
					}
					if err != nil {
						t.Fatalf("%s/%s ToJSON: %v", fx.Name, ver.tag, err)
					}
					var got, want any
					if err := json.Unmarshal(j, &got); err != nil {
						t.Fatalf("%s/%s unmarshal got: %v", fx.Name, ver.tag, err)
					}
					if err := json.Unmarshal(fx.Expected, &want); err != nil {
						t.Fatalf("expected: %v", err)
					}
					if !reflect.DeepEqual(normalizeJSON(t, got), normalizeJSON(t, want)) {
						t.Errorf("%s/%s mismatch:\n got=%#v\nwant=%#v", fx.Name, ver.tag, got, want)
					}
				}
			}
		})
	}
}
