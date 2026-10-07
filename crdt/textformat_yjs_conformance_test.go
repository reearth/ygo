package crdt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

type textFormatOp struct {
	P    string           `json:"p"`
	K    string           `json:"k"`
	I    int              `json:"i"`
	N    int              `json:"n"`
	S    string           `json:"s"`
	V    any              `json:"v"`
	A    *Attributes      `json:"a"`
	D    []map[string]any `json:"d"`
	From string           `json:"from"`
	To   string           `json:"to"`
	V2   bool             `json:"v2"`
}

type textFormatFixtures struct {
	Single []struct {
		Name  string          `json:"name"`
		Ops   []textFormatOp  `json:"ops"`
		V1    string          `json:"v1"`
		V2    string          `json:"v2"`
		Delta json.RawMessage `json:"delta"`
	} `json:"single"`
	Multi []struct {
		Name    string              `json:"name"`
		Clients map[string]ClientID `json:"clients"`
		Ops     []textFormatOp      `json:"ops"`
		Trace   [][]json.RawMessage `json:"trace"`
		Final   []struct {
			Peer string `json:"peer"`
			V1   string `json:"v1"`
		} `json:"final"`
	} `json:"multi"`
}

func loadTextFormatFixtures(t *testing.T) textFormatFixtures {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "textformat_yjs_fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx textFormatFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Single) == 0 || len(fx.Multi) == 0 {
		t.Fatal("no fixtures")
	}
	return fx
}

// textFormatDelta converts a Yjs JSON delta into ygo Deltas.
func textFormatDelta(raw []map[string]any) []Delta {
	out := make([]Delta, 0, len(raw))
	for _, m := range raw {
		var d Delta
		if v, ok := m["insert"]; ok {
			d.Op, d.Insert = DeltaOpInsert, jsNumbers(v)
		} else if v, ok := m["delete"]; ok {
			d.Op, d.Delete = DeltaOpDelete, int(v.(float64))
		} else if v, ok := m["retain"]; ok {
			d.Op, d.Retain = DeltaOpRetain, int(v.(float64))
		}
		if a, ok := m["attributes"]; ok {
			d.Attributes = jsNumbers(a).(map[string]any)
		}
		out = append(out, d)
	}
	return out
}

// applyTextFormatOp runs one script op; a missing `a` passes nil attributes.
func applyTextFormatOp(doc *Doc, o textFormatOp) {
	txt := doc.GetText("t")
	var a Attributes
	if o.A != nil {
		a = jsNumbers(map[string]any(*o.A)).(map[string]any)
	}
	doc.Transact(func(txn *Transaction) {
		switch o.K {
		case "ins":
			txt.Insert(txn, o.I, o.S, a)
		case "emb":
			txt.InsertEmbed(txn, o.I, jsNumbers(o.V), a)
		case "fmt":
			txt.Format(txn, o.I, o.N, a)
		case "del":
			txt.Delete(txn, o.I, o.N)
		case "delta":
			txt.ApplyDelta(txn, textFormatDelta(o.D))
		}
	})
}

// yjsDelta decodes a Yjs toDelta, joining adjacent text runs with equal
// attributes: Yjs splits a run at every live marker, even one that changes
// nothing, where ygo's ToDelta joins them.
func yjsDelta(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var ops []map[string]any
	if err := json.Unmarshal(raw, &ops); err != nil {
		t.Fatal(err)
	}
	out := make([]any, 0, len(ops))
	for _, op := range ops {
		if n := len(out); n > 0 {
			prev := out[n-1].(map[string]any)
			ps, ok1 := prev["insert"].(string)
			s, ok2 := op["insert"].(string)
			if ok1 && ok2 && reflect.DeepEqual(prev["attributes"], op["attributes"]) {
				prev["insert"] = ps + s
				continue
			}
		}
		out = append(out, op)
	}
	return out
}

// jsNumbers turns integral float64s into int64s, so a JSON-decoded 1 encodes
// as Yjs encodes the JS number 1 (an integer, not a float).
func jsNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<31 {
			return int64(x)
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = jsNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsNumbers(e)
		}
		return out
	}
	return v
}

// textFormatUnits lists a V1 update's structs, one clock unit per deleted
// struct, plus its compacted delete set. Yjs merges adjacent GC'd structs at
// commit and lists delete-set clients descending; ygo does neither, so two
// updates that differ only there list the same units.
func textFormatUnits(t *testing.T, u []byte) []string {
	t.Helper()
	structs, ds, err := decodeStructsV1(New(), u)
	if err != nil {
		t.Fatal(err)
	}
	id := func(p *ID) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprintf("%d:%d", p.Client, p.Clock)
	}
	clients := make([]ClientID, 0, len(structs))
	for c := range structs {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	var out []string
	for _, c := range clients {
		for _, it := range structs[c] {
			if cd, ok := it.Content.(*ContentDeleted); ok {
				for k := range cd.Len() {
					origin := it.Origin
					if k > 0 {
						origin = &ID{Client: c, Clock: it.ID.Clock + uint64(k) - 1}
					}
					out = append(out, fmt.Sprintf("%d:%d o=%s r=%s deleted", c, it.ID.Clock+uint64(k), id(origin), id(it.OriginRight)))
				}
				continue
			}
			out = append(out, fmt.Sprintf("%d:%d o=%s r=%s %T%+v", c, it.ID.Clock, id(it.Origin), id(it.OriginRight), it.Content, it.Content))
		}
	}
	dsClients := ds.Clients()
	sort.Slice(dsClients, func(i, j int) bool { return dsClients[i] < dsClients[j] })
	for _, c := range dsClients {
		ds.sortAndCompact(c)
		out = append(out, fmt.Sprintf("ds %d %v", c, ds.clients[c]))
	}
	return out
}

// sameUpdate reports whether got matches Yjs's want byte for byte or, failing
// that, unit for unit (see textFormatUnits).
func sameUpdate(t *testing.T, got, want []byte) bool {
	t.Helper()
	return bytes.Equal(got, want) || reflect.DeepEqual(textFormatUnits(t, got), textFormatUnits(t, want))
}

// Single-client scripts must match Yjs's content and V1/V2 state.
func TestConformance_TextFormat_Single(t *testing.T) {
	for _, fx := range loadTextFormatFixtures(t).Single {
		t.Run(fx.Name, func(t *testing.T) {
			doc := New(WithClientID(9))
			for _, o := range fx.Ops {
				applyTextFormatOp(doc, o)
			}
			want := yjsDelta(t, fx.Delta)
			if got := deltaAsYjsJSON(t, doc.GetText("t").ToDelta()); !reflect.DeepEqual(got, want) {
				t.Errorf("delta\n got=%v\nwant=%v", got, want)
			}
			if got := EncodeStateAsUpdateV1(doc, nil); !sameUpdate(t, got, mustHex(t, fx.V1)) {
				t.Errorf("V1\n got=%x\nwant=%s", got, fx.V1)
			}
			v2 := EncodeStateAsUpdateV2(doc, nil)
			if !bytes.Equal(v2, mustHex(t, fx.V2)) {
				asV1, err := UpdateV2ToV1(v2)
				if err != nil {
					t.Fatal(err)
				}
				if !sameUpdate(t, asV1, mustHex(t, fx.V1)) {
					t.Errorf("V2\n got=%x\nwant=%s", v2, fx.V2)
				}
			}
			for _, f := range []struct {
				hexed string
				apply func(*Doc, []byte, any) error
			}{{fx.V1, ApplyUpdateV1}, {fx.V2, ApplyUpdateV2}} {
				fresh := New(WithClientID(99))
				if err := f.apply(fresh, mustHex(t, f.hexed), nil); err != nil {
					t.Fatal(err)
				}
				if got := deltaAsYjsJSON(t, fresh.GetText("t").ToDelta()); !reflect.DeepEqual(got, want) {
					t.Errorf("Yjs state decoded as %v, want %v", got, want)
				}
			}
		})
	}
}

// Multi-peer scripts must match every peer's delta after every step, so a
// receiver's post-transaction format cleanup is checked where it
// happens, and each peer's final state must match Yjs's.
func TestConformance_TextFormat_MultiPeer(t *testing.T) {
	for _, fx := range loadTextFormatFixtures(t).Multi {
		t.Run(fx.Name, func(t *testing.T) {
			peers := make([]string, 0, len(fx.Clients))
			docs := map[string]*Doc{}
			for p, c := range fx.Clients {
				peers = append(peers, p)
				docs[p] = New(WithClientID(c))
			}
			sort.Strings(peers)
			for i, o := range fx.Ops {
				if o.K == "sync" {
					from, to := docs[o.From], docs[o.To]
					var err error
					if o.V2 {
						err = ApplyUpdateV2(to, EncodeStateAsUpdateV2(from, to.StateVector()), nil)
					} else {
						err = ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, to.StateVector()), nil)
					}
					if err != nil {
						t.Fatal(err)
					}
				} else {
					applyTextFormatOp(docs[o.P], o)
				}
				for j, p := range peers {
					want := yjsDelta(t, fx.Trace[i][j])
					if got := deltaAsYjsJSON(t, docs[p].GetText("t").ToDelta()); !reflect.DeepEqual(got, want) {
						t.Fatalf("step %d (%+v) peer %s\n got=%v\nwant=%v", i, o, p, got, want)
					}
				}
			}
			for _, f := range fx.Final {
				if got := EncodeStateAsUpdateV1(docs[f.Peer], nil); !sameUpdate(t, got, mustHex(t, f.V1)) {
					t.Errorf("peer %s final V1\n got=%v\nwant=%v", f.Peer, textFormatUnits(t, got), textFormatUnits(t, mustHex(t, f.V1)))
				}
			}
		})
	}
}
