package crdt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestCompat_MergeUpdates_GoToJS proves the struct-level MergeUpdatesV1/V2
// (#125, #57) produce updates real Yjs can apply and converge — including the
// non-integrable-struct case (a diff carrying B whose origin A lives in a prior
// update). Go merges the two updates; Yjs applies the single merged update and
// must see "AB". Skipped when node/yjs are unavailable.
func TestCompat_MergeUpdates_GoToJS(t *testing.T) {
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

	build := func() (updA, diffB []byte, svA crdt.StateVector) {
		d := crdt.New(crdt.WithClientID(1))
		txt := d.GetText("t")
		d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "A", nil) })
		svA = d.StateVector()
		updA = crdt.EncodeStateAsUpdateV1(d, nil)
		d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 1, "B", nil) })
		diffB = crdt.EncodeStateAsUpdateV1(d, svA) // carries only B (origin = A)
		return
	}

	updA, diffB, _ := build()
	mergedV1, err := crdt.MergeUpdatesV1(updA, diffB)
	require.NoError(t, err)

	// V2: build V2 equivalents directly and merge.
	dv2 := crdt.New(crdt.WithClientID(1))
	tv2 := dv2.GetText("t")
	dv2.Transact(func(tx *crdt.Transaction) { tv2.Insert(tx, 0, "A", nil) })
	svA2 := dv2.StateVector()
	updA2 := crdt.EncodeStateAsUpdateV2(dv2, nil)
	dv2.Transact(func(tx *crdt.Transaction) { tv2.Insert(tx, 1, "B", nil) })
	diffB2 := crdt.EncodeStateAsUpdateV2(dv2, svA2)
	mergedV2, err := crdt.MergeUpdatesV2(updA2, diffB2)
	require.NoError(t, err)

	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, b, 0644))
		return p
	}
	v1Path := write("merged_v1.bin", mergedV1)
	v2Path := write("merged_v2.bin", mergedV2)

	script := `
const Y = require(process.argv[2]);
const fs = require('fs');
const rd = p => new Uint8Array(fs.readFileSync(p));

const d1 = new Y.Doc();
Y.applyUpdate(d1, rd(process.argv[3]));
if (d1.getText('t').toString() !== 'AB') throw new Error('V1 merged text=' + d1.getText('t').toString());

const d2 = new Y.Doc();
Y.applyUpdateV2(d2, rd(process.argv[4]));
if (d2.getText('t').toString() !== 'AB') throw new Error('V2 merged text=' + d2.getText('t').toString());

console.log('OK');
`
	scriptPath := write("check.js", []byte(script))
	out, err := exec.Command(nodePath, scriptPath, yjsPath, v1Path, v2Path).CombinedOutput()
	t.Logf("node output:\n%s", out)
	require.NoError(t, err, "yjs failed to apply ygo's merged update — merge wire-format regression")
}
