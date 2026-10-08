package crdt_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// TestCompat_ContentJSON_GoToJS feeds ygo's re-encoding of yjs ContentJSON
// fixtures back to real yjs, full state and offset-split diffs, in V1 and V2.
func TestCompat_ContentJSON_GoToJS(t *testing.T) {
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	if err != nil {
		t.Fatal(err)
	}

	type fixture struct {
		Name      string          `json:"name"`
		Kind      string          `json:"kind"`
		V1        string          `json:"v1"`
		V2        string          `json:"v2"`
		Expected  json.RawMessage `json:"expected"`
		DiffClock *uint64         `json:"diffClock"`
	}
	type job struct {
		Name      string          `json:"name"`
		Kind      string          `json:"kind"`
		Expected  json.RawMessage `json:"expected"`
		DiffClock *uint64         `json:"diffClock"`
		Full      [2]string       `json:"full"`
		Diff      [2]string       `json:"diff"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "contentjson_yjs_fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fxs []fixture
	if err := json.Unmarshal(raw, &fxs); err != nil {
		t.Fatal(err)
	}

	jobs := make([]job, 0, len(fxs))
	for _, fx := range fxs {
		// Source each direction from the other wire version, so a decode-side
		// and encode-side bug cannot cancel out.
		docs := [2]*crdt.Doc{crdt.New(), crdt.New()}
		for i, src := range []struct {
			hexed string
			apply func(*crdt.Doc, []byte, any) error
		}{{fx.V2, crdt.ApplyUpdateV2}, {fx.V1, crdt.ApplyUpdateV1}} {
			b, _ := hex.DecodeString(src.hexed)
			if err := src.apply(docs[i], b, nil); err != nil {
				t.Fatalf("%s: decode: %v", fx.Name, err)
			}
		}
		j := job{Name: fx.Name, Kind: fx.Kind, Expected: fx.Expected, DiffClock: fx.DiffClock}
		j.Full = [2]string{
			hex.EncodeToString(crdt.EncodeStateAsUpdateV1(docs[0], nil)),
			hex.EncodeToString(crdt.EncodeStateAsUpdateV2(docs[1], nil)),
		}
		if fx.DiffClock != nil {
			sv := crdt.StateVector{7: *fx.DiffClock}
			j.Diff = [2]string{
				hex.EncodeToString(crdt.EncodeStateAsUpdateV1(docs[0], sv)),
				hex.EncodeToString(crdt.EncodeStateAsUpdateV2(docs[1], sv)),
			}
		}
		jobs = append(jobs, j)
	}

	dir := t.TempDir()
	in, _ := json.Marshal(jobs)
	inPath := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(inPath, in, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
const Y = require(process.argv[2]);
const { isDeepStrictEqual } = require('util');
const jobs = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const u8 = (h) => new Uint8Array(Buffer.from(h, 'hex'));
const norm = (v) => JSON.parse(JSON.stringify(v, (k, x) => (x === undefined ? null : x)));
let bad = 0;
for (const j of jobs) {
  [['v1', Y.applyUpdate, Y.decodeUpdate], ['v2', Y.applyUpdateV2, Y.decodeUpdateV2]].forEach(([tag, apply, decode], i) => {
    try {
      const d = new Y.Doc();
      apply(d, u8(j.full[i]));
      const got = norm({ map: d.getMap('m'), array: d.getArray('a'), xml: d.getXmlFragment('x') }[j.kind].toJSON());
      if (!isDeepStrictEqual(got, j.expected)) throw new Error('full: ' + JSON.stringify(got));
      if (j.diffClock != null) {
        const vals = decode(u8(j.diff[i])).structs.flatMap((s) => s.content.getContent());
        if (!isDeepStrictEqual(norm(vals), j.expected.slice(j.diffClock))) throw new Error('diff: ' + JSON.stringify(vals));
      }
    } catch (e) {
      bad++;
      console.log(j.name + '/' + tag + ': ' + e.message);
    }
  });
}
if (bad) process.exit(1);
console.log('OK');
`
	scriptPath := filepath.Join(dir, "check.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nodePath, scriptPath, yjsPath, inPath).CombinedOutput()
	t.Logf("node output:\n%s", out)
	if err != nil {
		t.Fatalf("yjs rejected or misread ygo's ContentJSON encoding: %v", err)
	}
}
