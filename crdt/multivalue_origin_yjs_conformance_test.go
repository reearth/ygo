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

type multiValueFixture struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	GC   bool   `json:"gc"`
	V1   string `json:"v1"`
	V2   string `json:"v2"`
	Sets []struct {
		Client   crdt.ClientID   `json:"client"`
		Update   string          `json:"update"`
		Expected json.RawMessage `json:"expected"`
	} `json:"sets"`
}

// multiValueContent reads the fixture's root as Yjs's toJSON / toString would.
func multiValueContent(t *testing.T, doc *crdt.Doc, kind string) any {
	t.Helper()
	if kind == "xmlattr" {
		return doc.GetXmlFragment("x").ToXML()
	}
	j, err := doc.GetMap("m").ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// multiValueSet writes k=3; shared types are fetched outside Transact, which
// holds the doc lock.
func multiValueSet(doc *crdt.Doc, kind string) {
	if kind == "xmlattr" {
		p := doc.GetXmlFragment("x").Children()[0].(*crdt.YXmlElement)
		doc.Transact(func(txn *crdt.Transaction) { p.SetAttribute(txn, "k", "3") })
		return
	}
	m := doc.GetMap("m")
	doc.Transact(func(txn *crdt.Transaction) { m.Set(txn, "k", 3) })
}

// TestConformance_MultiValueEntry_SetOrigin overwrites a key whose deleted
// history is one merged multi-value struct, from a clientID below and above
// the author's: ygo must emit Yjs's bytes (origin = the run's last id), and a
// fresh peer must read the new value, not lose the key.
func TestConformance_MultiValueEntry_SetOrigin(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "multivalue_yjs_fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fxs []multiValueFixture
	if err := json.Unmarshal(raw, &fxs); err != nil {
		t.Fatal(err)
	}
	if len(fxs) == 0 {
		t.Fatal("no fixtures")
	}
	for _, fx := range fxs {
		for _, set := range fx.Sets {
			var want any
			if err := json.Unmarshal(set.Expected, &want); err != nil {
				t.Fatal(err)
			}
			for _, src := range []struct {
				tag   string
				hexed string
				apply func(*crdt.Doc, []byte, any) error
			}{{"v1", fx.V1, crdt.ApplyUpdateV1}, {"v2", fx.V2, crdt.ApplyUpdateV2}} {
				for _, gc := range []bool{true, false} {
					base, _ := hex.DecodeString(src.hexed)
					doc := crdt.New(crdt.WithClientID(set.Client), crdt.WithGC(gc))
					if err := src.apply(doc, base, nil); err != nil {
						t.Fatalf("%s/%s: %v", fx.Name, src.tag, err)
					}
					sv := doc.StateVector()
					multiValueSet(doc, fx.Kind)
					tag := fx.Name + "/" + src.tag
					if got := hex.EncodeToString(crdt.EncodeStateAsUpdateV1(doc, sv)); got != set.Update {
						t.Errorf("%s client=%d gc=%v: update\n got=%s\nwant=%s", tag, set.Client, gc, got, set.Update)
					}
					if got := multiValueContent(t, doc, fx.Kind); !reflect.DeepEqual(got, want) {
						t.Errorf("%s client=%d gc=%v: local %v, want %v", tag, set.Client, gc, got, want)
					}
					for _, enc := range []struct {
						encode func(*crdt.Doc, crdt.StateVector) []byte
						apply  func(*crdt.Doc, []byte, any) error
					}{{crdt.EncodeStateAsUpdateV1, crdt.ApplyUpdateV1}, {crdt.EncodeStateAsUpdateV2, crdt.ApplyUpdateV2}} {
						fresh := crdt.New(crdt.WithClientID(99))
						if err := enc.apply(fresh, enc.encode(doc, nil), nil); err != nil {
							t.Fatal(err)
						}
						if got := multiValueContent(t, fresh, fx.Kind); !reflect.DeepEqual(got, want) {
							t.Errorf("%s client=%d gc=%v: fresh peer %v, want %v", tag, set.Client, gc, got, want)
						}
					}
				}
			}
		}
	}
}
