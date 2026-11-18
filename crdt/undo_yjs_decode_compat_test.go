package crdt_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestCompat_UndoRegressedBytesDecodeInYjs has Yjs apply the V1 and V2 updates
// ygo writes for each undo fixture whose bytes differ from Yjs's, and requires
// the content Yjs reached when it ran the same steps.
func TestCompat_UndoRegressedBytesDecodeInYjs(t *testing.T) {
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	require.NoError(t, err)

	replays := crdt.UndoRegressedReplays(t)
	require.NotEmpty(t, replays)
	dir := t.TempDir()
	input, err := json.Marshal(replays)
	require.NoError(t, err)
	inputPath := filepath.Join(dir, "replays.json")
	require.NoError(t, os.WriteFile(inputPath, input, 0o644))
	script := `
const Y = require(process.argv[2])
const replays = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'))
let failed = false
for (const r of replays) {
  for (const [form, b64, apply] of [['V1', r.V1, Y.applyUpdate], ['V2', r.V2, Y.applyUpdateV2]]) {
    const doc = new Y.Doc()
    apply(doc, new Uint8Array(Buffer.from(b64, 'base64')))
    const root = r.Kind === 'map' ? doc.getMap(r.Root) : doc.getArray(r.Root)
    const got = JSON.stringify(root.toJSON())
    const want = JSON.stringify(JSON.parse(r.ExpectedJSON))
    console.log(r.Name + ' ' + form + ': yjs reads ' + got + ', want ' + want)
    if (got !== want) failed = true
  }
}
if (failed) process.exit(1)
`
	scriptPath := filepath.Join(dir, "check.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))
	out, err := exec.Command(nodePath, scriptPath, yjsPath, inputPath).CombinedOutput()
	t.Logf("node output:\n%s", out)
	require.NoError(t, err, "Yjs reads other content from ygo's update")
}
