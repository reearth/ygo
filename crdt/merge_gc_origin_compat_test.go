package crdt_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestCompat_MergeUpdates_GCOriginsMatchYjs has Yjs merge and diff the same
// updates as ygo, where client 7's "b" has an origin or right origin in a GC
// struct, and requires both outputs to carry b alike and show the same text.
func TestCompat_MergeUpdates_GCOriginsMatchYjs(t *testing.T) {
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	require.NoError(t, err)

	a := crdt.New(crdt.WithClientID(5))
	text := a.GetText("t")
	a.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "hello", nil) })
	hello := crdt.EncodeStateAsUpdateV1(a, nil)
	a.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 2, "ZZ", nil) })
	gc := []byte{1, 1, 5, 5, 0, 2, 0} // one GC struct over client 5's "ZZ"

	type row struct {
		Name   string   `json:"name"`
		V2     bool     `json:"v2"`
		Inputs [][]byte `json:"inputs"`
		Merged []byte   `json:"merged"`
		Diff   []byte   `json:"diff"`
	}
	positions := []int{2, 3, 4}
	rows := make([]row, 0, len(positions)*len(updateFormats))
	for _, pos := range positions {
		b := crdt.New(crdt.WithClientID(7))
		require.NoError(t, crdt.ApplyUpdateV1(b, crdt.EncodeStateAsUpdateV1(a, nil), nil))
		sv := b.StateVector()
		bText := b.GetText("t")
		b.Transact(func(txn *crdt.Transaction) { bText.Insert(txn, pos, "b", nil) })
		inputs := [][]byte{hello, gc, crdt.EncodeStateAsUpdateV1(b, sv)}
		for _, f := range updateFormats {
			var r row
			r.Name, r.V2 = fmt.Sprintf("%s b at %d", f.name, pos), f.name == "V2"
			for _, u := range inputs {
				if r.V2 {
					u, err = crdt.UpdateV1ToV2(u)
					require.NoError(t, err)
				}
				r.Inputs = append(r.Inputs, u)
			}
			merge, diff := crdt.MergeUpdatesV1, crdt.DiffUpdateV1
			if r.V2 {
				merge, diff = crdt.MergeUpdatesV2, crdt.DiffUpdateV2
			}
			r.Merged, err = merge(r.Inputs...)
			require.NoError(t, err)
			r.Diff, err = diff(r.Merged, crdt.StateVector{5: 5})
			require.NoError(t, err)
			rows = append(rows, r)
		}
	}

	dir := t.TempDir()
	input, err := json.Marshal(rows)
	require.NoError(t, err)
	inputPath := filepath.Join(dir, "rows.json")
	require.NoError(t, os.WriteFile(inputPath, input, 0o644))
	script := `
const Y = require(process.argv[2]);
const rows = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const bytes = (b64) => new Uint8Array(Buffer.from(b64, 'base64'));
const client7 = (r, u) => JSON.stringify((r.v2 ? Y.decodeUpdateV2 : Y.decodeUpdate)(u).structs
  .filter((s) => s.id.client === 7)
  .map((s) => [s.constructor.name, s.id.clock, s.length, s.origin, s.rightOrigin]));
const text = (r, ...us) => {
  const doc = new Y.Doc();
  for (const u of us) (r.v2 ? Y.applyUpdateV2 : Y.applyUpdate)(doc, u);
  return doc.getText('t').toString();
};
let failed = false;
for (const r of rows) {
  const inputs = r.inputs.map(bytes);
  const merged = (r.v2 ? Y.mergeUpdatesV2 : Y.mergeUpdates)(inputs);
  const sv = (r.v2 ? Y.encodeStateVectorFromUpdateV2 : Y.encodeStateVectorFromUpdate)(inputs[0]);
  const diff = (r.v2 ? Y.diffUpdateV2 : Y.diffUpdate)(merged, sv);
  const checks = [
    ['merge', client7(r, merged), client7(r, bytes(r.merged)), text(r, merged), text(r, bytes(r.merged))],
    ['diff', client7(r, diff), client7(r, bytes(r.diff)), text(r, inputs[0], diff), text(r, inputs[0], bytes(r.diff))],
  ];
  for (const [what, yjs, ygo, yjsText, ygoText] of checks) {
    console.log(r.name + ' ' + what + ': yjs ' + yjs + ' ' + JSON.stringify(yjsText) + ', ygo ' + ygo + ' ' + JSON.stringify(ygoText));
    if (yjs !== ygo || yjsText !== ygoText) failed = true;
  }
}
if (failed) process.exit(1);
`
	scriptPath := filepath.Join(dir, "check.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))
	out, err := exec.Command(nodePath, scriptPath, yjsPath, inputPath).CombinedOutput()
	t.Logf("node output:\n%s", out)
	require.NoError(t, err, "ygo's merged or diffed update differs from Yjs's")
}
