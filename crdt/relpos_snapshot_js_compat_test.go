package crdt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestCompat_RelPosAndSnapshot_GoToJS proves the v1.23.1 wire-format fixes
// (review C-4 RelativePosition tags, F-5 Snapshot layout) interoperate with the
// real Yjs reference implementation: Go encodes, Node/Yjs decodes and resolves.
// Skipped when node is unavailable (headless CI).
func TestCompat_RelPosAndSnapshot_GoToJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found on PATH — skipping Go→JS interop test")
	}
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(yjsPath); err != nil {
		t.Skip("yjs not installed under testutil/node_modules — skipping")
	}

	// Build a doc: YText "t" = "hello" (client 1).
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hello", nil) })

	state := crdt.EncodeStateAsUpdateV1(doc, nil)

	// Item-anchored relative position at index 2; must resolve back to 2 in Yjs.
	rpItem := crdt.EncodeRelativePosition(crdt.CreateRelativePositionFromIndex(txt, 2, 0))
	// Tname-anchored position (no item; assoc=0) — a start-of-type anchor.
	rpTname := crdt.EncodeRelativePosition(crdt.RelativePosition{Tname: "t", Assoc: 0})
	// Snapshot of the current state (ds empty, sv {1:5}).
	snap := crdt.EncodeSnapshot(crdt.CaptureSnapshot(doc))

	dir := t.TempDir()
	writeFile := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, b, 0644))
		return p
	}
	statePath := writeFile("state.bin", state)
	rpItemPath := writeFile("rp_item.bin", rpItem)
	rpTnamePath := writeFile("rp_tname.bin", rpTname)
	snapPath := writeFile("snap.bin", snap)

	script := `
const Y = require(process.argv[2]);
const fs = require('fs');
const rd = p => new Uint8Array(fs.readFileSync(p));
const doc = new Y.Doc();
Y.applyUpdate(doc, rd(process.argv[3]));
const t = doc.getText('t');
if (t.toString() !== 'hello') throw new Error('text=' + t.toString());

// C-4: item-anchored relative position resolves back to index 2.
const rpItem = Y.decodeRelativePosition(rd(process.argv[4]));
const absItem = Y.createAbsolutePositionFromRelativePosition(rpItem, doc);
if (!absItem) throw new Error('item abs is null');
if (absItem.index !== 2) throw new Error('item index=' + absItem.index + ' want 2');

// C-4: tname-anchored relative position decodes to tname='t' and resolves to a
// valid absolute position. (Yjs resolves a null-item/tname anchor to the end of
// the type; ygo's ToAbsolutePosition treats it as the start — a resolution
// semantics difference tracked separately. This test only proves the WIRE
// format: that Yjs can decode what ygo encoded.)
const rpTname = Y.decodeRelativePosition(rd(process.argv[5]));
if (rpTname.tname !== 't') throw new Error('tname=' + rpTname.tname);
const absTname = Y.createAbsolutePositionFromRelativePosition(rpTname, doc);
if (!absTname) throw new Error('tname abs is null');

// F-5: snapshot decodes; sv has client 1 -> clock 5; delete set empty.
const snap = Y.decodeSnapshot(rd(process.argv[6]));
if (snap.sv.get(1) !== 5) throw new Error('snap sv[1]=' + snap.sv.get(1) + ' want 5');
if (snap.ds.clients.size !== 0) throw new Error('snap ds not empty: ' + snap.ds.clients.size);

console.log('OK');
`
	scriptPath := writeFile("check.js", []byte(script))

	out, err := exec.Command(nodePath, scriptPath, yjsPath, statePath, rpItemPath, rpTnamePath, snapPath).CombinedOutput()
	t.Logf("node output:\n%s", out)
	require.NoError(t, err, "yjs failed to decode ygo-encoded RelativePosition/Snapshot — wire-format regression")
}
