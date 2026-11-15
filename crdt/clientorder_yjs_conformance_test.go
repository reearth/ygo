package crdt

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type clientOrderOp struct {
	Peer int    `json:"peer"`
	Op   string `json:"op"`
	Pos  int    `json:"pos"`
	N    int    `json:"n"`
	S    string `json:"s"`
	Key  string `json:"key"`
	From int    `json:"from"`
	To   int    `json:"to"`
}

type clientOrderFixture struct {
	Name     string            `json:"name"`
	GC       bool              `json:"gc"`
	Clients  []ClientID        `json:"clients"`
	Ops      []clientOrderOp   `json:"ops"`
	SV       string            `json:"sv"`
	Local    []string          `json:"local"`
	LocalV2  []string          `json:"localV2"`
	Expected map[string]string `json:"expected"`
}

// Every encoder must write clients in yjs's order, descending, so a
// multi-client update or state vector is byte-identical to yjs's.
func TestCompat_ClientOrder_MatchesYjs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "clientorder_yjs_fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []clientOrderFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	for _, fx := range fixtures {
		t.Run(fx.Name, func(t *testing.T) {
			docs := make([]*Doc, len(fx.Clients))
			for i, c := range fx.Clients {
				docs[i] = New(WithClientID(c), WithGC(fx.GC))
			}
			full := docs[len(docs)-1]
			var snap *Snapshot
			for _, o := range fx.Ops {
				switch o.Op {
				case "sync":
					syncTo(t, docs[o.From], docs[o.To])
					continue
				case "snapshot":
					snap = CaptureSnapshot(full)
					continue
				}
				d := docs[o.Peer]
				text, m, arr := d.GetText("t"), d.GetMap("m"), d.GetArray("a")
				d.Transact(func(txn *Transaction) {
					switch o.Op {
					case "ins":
						text.Insert(txn, o.Pos, o.S, nil)
					case "del":
						text.Delete(txn, o.Pos, o.N)
					case "mset":
						m.Set(txn, o.Key, o.S)
					case "ains":
						arr.Insert(txn, o.Pos, []any{o.S})
					}
				})
			}

			v1, v2 := mustHex(t, fx.Expected["stateV1"]), mustHex(t, fx.Expected["stateV2"])
			sv, err := DecodeStateVectorV1(mustHex(t, fx.SV))
			if err != nil {
				t.Fatal(err)
			}
			fresh := New(WithClientID(5), WithGC(fx.GC))
			var received []byte
			fresh.OnUpdate(func(u []byte, _ any) { received = u })
			if err := ApplyUpdateV1(fresh, v1, nil); err != nil {
				t.Fatal(err)
			}
			local := make([][]byte, len(fx.Local))
			for i, h := range fx.Local {
				local[i] = mustHex(t, h)
			}
			localV2 := make([][]byte, len(fx.LocalV2))
			for i, h := range fx.LocalV2 {
				localV2[i] = mustHex(t, h)
			}
			must := func(b []byte, err error) []byte {
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			got := map[string][]byte{
				"stateV1":           EncodeStateAsUpdateV1(full, nil),
				"stateV2":           EncodeStateAsUpdateV2(full, nil),
				"stateVector":       EncodeStateVectorV1(full),
				"snapshot":          EncodeSnapshot(CaptureSnapshot(full)),
				"diffStateV1":       EncodeStateAsUpdateV1(full, sv),
				"diffStateV2":       EncodeStateAsUpdateV2(full, sv),
				"received":          received,
				"mergeV1":           must(MergeUpdatesV1(local...)),
				"mergeV2":           must(MergeUpdatesV2(localV2...)),
				"diffUpdateV1":      must(DiffUpdateV1(v1, sv)),
				"diffUpdateV2":      must(DiffUpdateV2(v2, sv)),
				"v1ToV2":            must(UpdateV1ToV2(v1)),
				"v2ToV1":            must(UpdateV2ToV1(v2)),
				"stateVectorFromV1": must(EncodeStateVectorFromUpdate(v1)),
				"stateVectorFromV2": must(EncodeStateVectorFromUpdateV2(v2)),
			}
			if !fx.GC {
				got["fromSnapshot"] = must(EncodeStateFromSnapshot(full, snap))
			}
			if len(got) != len(fx.Expected) {
				t.Fatalf("checked %d encoders, fixture has %d", len(got), len(fx.Expected))
			}
			for name, want := range fx.Expected {
				if g := hex.EncodeToString(got[name]); g != want {
					t.Errorf("%s:\n got=%s\nwant=%s", name, g, want)
				}
			}
		})
	}
}
