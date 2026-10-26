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

type multiValueSetFixture struct {
	Client   crdt.ClientID   `json:"client"`
	Update   string          `json:"update"`
	Expected json.RawMessage `json:"expected"`
}

type multiValueFixture struct {
	Name  string                 `json:"name"`
	Kind  string                 `json:"kind"`
	GC    bool                   `json:"gc"`
	V1    string                 `json:"v1"`
	V2    string                 `json:"v2"`
	Sets  []multiValueSetFixture `json:"sets"`
	Split struct {
		Update string                 `json:"update"`
		Sets   []multiValueSetFixture `json:"sets"`
	} `json:"split"`
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

// multiValueUpdate hex-encodes u; a multi-client delete set is re-encoded
// through MergeUpdatesV1, since ygo's V1 orders its clients ascending and Yjs
// descending.
func multiValueUpdate(t *testing.T, u []byte, canonical bool) string {
	t.Helper()
	if canonical {
		var err error
		if u, err = crdt.MergeUpdatesV1(u); err != nil {
			t.Fatal(err)
		}
	}
	return hex.EncodeToString(u)
}

// TestConformance_MultiValueEntry_SetOrigin overwrites a key whose deleted
// history is one merged multi-value struct, from a clientID below and above
// the author's: ygo must emit Yjs's bytes (origin = the run's last id), and a
// fresh peer must read the new value, not lose the key. The split variant
// first applies a write whose origin is the run's first id, splitting it;
// the key must then track the run's right half.
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
		if fx.Split.Update == "" || len(fx.Split.Sets) == 0 {
			t.Fatalf("%s: no split scenario", fx.Name)
		}
		split, _ := hex.DecodeString(fx.Split.Update)
		for _, sc := range []struct {
			tag   string
			extra []byte
			sets  []multiValueSetFixture
		}{{"", nil, fx.Sets}, {"/split", split, fx.Split.Sets}} {
			for _, set := range sc.sets {
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
						tag := fx.Name + sc.tag + "/" + src.tag
						if sc.extra != nil {
							if err := crdt.ApplyUpdateV1(doc, sc.extra, nil); err != nil {
								t.Fatalf("%s: %v", tag, err)
							}
						}
						sv := doc.StateVector()
						multiValueSet(doc, fx.Kind)
						if got := multiValueUpdate(t, crdt.EncodeStateAsUpdateV1(doc, sv), sc.extra != nil); got != multiValueUpdate(t, mustHex(t, set.Update), sc.extra != nil) {
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
}
