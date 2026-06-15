package crdt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestCompat_FormatNegatedAttrs_GoToJS proves the F-7 formatText port (#123)
// produces a document whose formatting Yjs renders identically: Go applies the
// Format operations, encodes the state, and real Yjs decodes it and asserts the
// resulting toDelta matches — across the rebold-subrange and unset-middle cases
// (the scenarios that the pre-fix over-delete got wrong). Skipped when node /
// yjs are unavailable (headless CI).
func TestCompat_FormatNegatedAttrs_GoToJS(t *testing.T) {
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

	doc := crdt.New(crdt.WithClientID(1))
	// t1: rebold a sub-range of an already-bold run → stays all bold.
	t1 := doc.GetText("t1")
	// t2: unset a middle sub-range of a bold run → bold restored after the gap.
	t2 := doc.GetText("t2")
	// t3: format overlapping at a boundary → both runs stay bold.
	t3 := doc.GetText("t3")
	doc.Transact(func(txn *crdt.Transaction) {
		t1.Insert(txn, 0, "aaabbb", nil)
		t1.Format(txn, 0, 6, crdt.Attributes{"bold": true})
		t1.Format(txn, 0, 3, crdt.Attributes{"bold": true})

		t2.Insert(txn, 0, "aaabbb", nil)
		t2.Format(txn, 0, 6, crdt.Attributes{"bold": true})
		t2.Format(txn, 2, 2, crdt.Attributes{"bold": nil})

		t3.Insert(txn, 0, "abc DEF ghi", nil)
		t3.Format(txn, 4, 3, crdt.Attributes{"bold": true})
		t3.Format(txn, 0, 4, crdt.Attributes{"bold": true})
	})

	state := crdt.EncodeStateAsUpdateV1(doc, nil)
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.bin")
	require.NoError(t, os.WriteFile(statePath, state, 0644))

	script := `
const Y = require(process.argv[2]);
const fs = require('fs');
const doc = new Y.Doc();
Y.applyUpdate(doc, new Uint8Array(fs.readFileSync(process.argv[3])));
const eq = (got, want, label) => {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) throw new Error(label + ': got ' + g + ' want ' + w);
};
eq(doc.getText('t1').toDelta(), [{insert:'aaabbb',attributes:{bold:true}}], 't1 rebold');
eq(doc.getText('t2').toDelta(), [{insert:'aa',attributes:{bold:true}},{insert:'ab'},{insert:'bb',attributes:{bold:true}}], 't2 unset-middle');
eq(doc.getText('t3').toDelta(), [{insert:'abc DEF',attributes:{bold:true}},{insert:' ghi'}], 't3 overlap-boundary');
console.log('OK');
`
	scriptPath := filepath.Join(dir, "check.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0644))

	out, err := exec.Command(nodePath, scriptPath, yjsPath, statePath).CombinedOutput()
	t.Logf("node output:\n%s", out)
	require.NoError(t, err, "yjs rendered ygo's formatted document differently — formatText parity regression")
}
