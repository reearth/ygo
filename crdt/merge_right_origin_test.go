package crdt_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// ygo merges adjacent same-client items into one struct, and an encoder writes
// that struct with the left item's origin and right origin. Yjs's
// Item.mergeWith therefore merges two items only when the right one was
// inserted directly after the left one's last character and both share a right
// origin. Merging items whose right origins differ moves the right item's
// characters the next time the document is encoded and decoded.

type updateFormat struct {
	name   string
	encode func(*crdt.Doc, crdt.StateVector) []byte
	apply  func(*crdt.Doc, []byte, any) error
}

var updateFormats = []updateFormat{
	{"V1", crdt.EncodeStateAsUpdateV1, crdt.ApplyUpdateV1},
	{"V2", crdt.EncodeStateAsUpdateV2, crdt.ApplyUpdateV2},
}

// rightOriginFork builds "cZb": client 2 inserts "c", client 1 inserts "b"
// after it, then client 2 inserts "Z" between them. "c" (2:0) has no right
// origin; "Z" (2:1) has origin 2:0 and right origin "b" (1:0). The two client-2
// items are adjacent and clock-contiguous but must not be merged.
func rightOriginFork(t *testing.T) *crdt.Doc {
	t.Helper()
	d2 := crdt.New(crdt.WithClientID(2))
	t2 := d2.GetText("t")
	d2.Transact(func(txn *crdt.Transaction) { t2.Insert(txn, 0, "c", nil) })
	d1 := crdt.New(crdt.WithClientID(1))
	require.NoError(t, crdt.ApplyUpdateV1(d1, crdt.EncodeStateAsUpdateV1(d2, nil), nil))
	t1 := d1.GetText("t")
	d1.Transact(func(txn *crdt.Transaction) { t1.Insert(txn, 1, "b", nil) })
	require.NoError(t, crdt.ApplyUpdateV1(d2, crdt.EncodeStateAsUpdateV1(d1, nil), nil))
	d2.Transact(func(txn *crdt.Transaction) { t2.Insert(txn, 1, "Z", nil) })
	require.Equal(t, "cZb", t2.ToString())
	return d2
}

// reencodeAfterOneApply applies update to a fresh document in one transaction
// and returns that document's own encoding of its state.
func reencodeAfterOneApply(t *testing.T, f updateFormat, update []byte) []byte {
	t.Helper()
	doc := crdt.New()
	require.NoError(t, f.apply(doc, update, nil))
	return f.encode(doc, nil)
}

func readText(t *testing.T, f updateFormat, update []byte) string {
	t.Helper()
	doc := crdt.New()
	require.NoError(t, f.apply(doc, update, nil))
	return doc.GetText("t").ToString()
}

func TestUnit_ApplyUpdate_ReencodingKeepsRightOrigins(t *testing.T) {
	for _, f := range updateFormats {
		t.Run(f.name, func(t *testing.T) {
			source := f.encode(rightOriginFork(t), nil)
			reencoded := reencodeAfterOneApply(t, f, source)
			require.Equal(t, "cZb", readText(t, f, reencoded),
				"a document loaded in one apply must encode Z with its own right origin")
		})
	}
}

// foldedLogRows is a log of ten V1 updates from three clients, in the order a
// server stored them. Row 8 inserts "  ch" (103:5-8) with origin 103:4 and
// right origin 100:4, next to 103:4, whose right origin is 103:3. Yjs 13.6.32
// reads the log as "hc  chb", both row by row and after folding rows 0-8 into
// one state.
var foldedLogRows = []string{
	"AQFlAAQBAXQBYwA=",
	"AQFkAAgBAWECdwRlZyBnfEBAAAABZQEAAQ==",
	"AQJnAIdkAQEoAGcAAXYBdwZlIGVhaGcBZQEAAQ==",
	"AQFkAohnAAJ3BiBlIGhjZXxAoAAAAWUBAAE=",
	"AQFmAIRlAAFoAWUBAAE=",
	"AQJnAsZlAGYABGJvbGQEdHJ1ZYZmAARib2xkBG51bGwBZQEAAQ==",
	"AQFnBMRmAGcDAWMBZQEAAQ==",
	"AQFkBMRnBGcDAWIBZQEAAQ==",
	"AQFnBcRnBGQEBCAgY2gBZQEAAQ==",
	"AAFlAQAB",
}

// foldLog merges rows 0-8 and folds them into one state the way a compaction
// does (one apply, re-encode). It returns the merge, the folded state and row 9.
func foldLog(t *testing.T) (merged, folded, last []byte) {
	t.Helper()
	rows := make([][]byte, len(foldedLogRows))
	for i, row := range foldedLogRows {
		update, err := base64.StdEncoding.DecodeString(row)
		require.NoError(t, err)
		rows[i] = update
	}
	merged, err := crdt.MergeUpdatesV1(rows[:9]...)
	require.NoError(t, err)
	return merged, reencodeAfterOneApply(t, updateFormats[0], merged), rows[9]
}

func TestUnit_ApplyUpdate_FoldedLogKeepsItsOrderThroughALaterUpdate(t *testing.T) {
	_, folded, last := foldLog(t)
	doc := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(doc, folded, nil))
	require.NoError(t, crdt.ApplyUpdateV1(doc, last, nil))
	require.Equal(t, "hc  chb", doc.GetText("t").ToString())
}

// gcFork builds "cZQb" on client 2's own document, where client 3 inserted "Q"
// with origin "Z" (2:1), then deletes "c" and "Z". The two tombstones are
// adjacent and clock-contiguous, but "Z" has right origin "b" and "c" has none.
func gcFork(t *testing.T) *crdt.Doc {
	t.Helper()
	d2 := rightOriginFork(t)
	d3 := crdt.New(crdt.WithClientID(3))
	require.NoError(t, crdt.ApplyUpdateV1(d3, crdt.EncodeStateAsUpdateV1(d2, nil), nil))
	t3 := d3.GetText("t")
	d3.Transact(func(txn *crdt.Transaction) { t3.Insert(txn, 2, "Q", nil) })
	require.NoError(t, crdt.ApplyUpdateV1(d2, crdt.EncodeStateAsUpdateV1(d3, nil), nil))
	text := d2.GetText("t")
	d2.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, 2) })
	require.Equal(t, "Qb", text.ToString())
	return d2
}

func TestUnit_RunGC_KeepsRightOriginsOfAdjacentTombstones(t *testing.T) {
	doc := gcFork(t)
	crdt.RunGC(doc)
	require.Equal(t, "Qb", readText(t, updateFormats[0], crdt.EncodeStateAsUpdateV1(doc, nil)),
		"merging the tombstones of c and Z must not move Q, whose origin is Z")
}

// TestCompat_MergeRightOrigin_YjsReadsReencodedStates has real Yjs read each
// source state and ygo's re-encoding of it, and requires the same text from
// both.
func TestCompat_MergeRightOrigin_YjsReadsReencodedStates(t *testing.T) {
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	require.NoError(t, err)

	type pair struct {
		Name      string `json:"name"`
		V2        bool   `json:"v2"`
		Source    []byte `json:"source"`
		Reencoded []byte `json:"reencoded"`
		Last      []byte `json:"last,omitempty"`
	}
	pairs := make([]pair, 0, len(updateFormats)+2)
	for _, f := range updateFormats {
		source := f.encode(rightOriginFork(t), nil)
		pairs = append(pairs, pair{
			Name: "one apply " + f.name, V2: f.name == "V2",
			Source: source, Reencoded: reencodeAfterOneApply(t, f, source),
		})
	}
	gc := gcFork(t)
	beforeGC := crdt.EncodeStateAsUpdateV1(gc, nil)
	crdt.RunGC(gc)
	pairs = append(pairs, pair{Name: "RunGC", Source: beforeGC, Reencoded: crdt.EncodeStateAsUpdateV1(gc, nil)})
	merged, folded, last := foldLog(t)
	pairs = append(pairs, pair{Name: "folded log", Source: merged, Reencoded: folded, Last: last})

	dir := t.TempDir()
	input, err := json.Marshal(pairs)
	require.NoError(t, err)
	inputPath := filepath.Join(dir, "pairs.json")
	require.NoError(t, os.WriteFile(inputPath, input, 0o644))
	script := `
const Y = require(process.argv[2]);
const pairs = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const read = (p, b64) => {
  const doc = new Y.Doc();
  (p.v2 ? Y.applyUpdateV2 : Y.applyUpdate)(doc, Buffer.from(b64, 'base64'));
  if (p.last) Y.applyUpdate(doc, Buffer.from(p.last, 'base64'));
  return doc.getText('t').toString();
};
let failed = false;
for (const p of pairs) {
  const want = read(p, p.source), got = read(p, p.reencoded);
  console.log(p.name + ': source ' + JSON.stringify(want) + ', ygo re-encoding ' + JSON.stringify(got));
  if (got !== want) failed = true;
}
if (failed) process.exit(1);
`
	scriptPath := filepath.Join(dir, "check.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))
	out, err := exec.Command(nodePath, scriptPath, yjsPath, inputPath).CombinedOutput()
	t.Logf("node output:\n%s", out)
	require.NoError(t, err, "Yjs reads ygo's re-encoding differently from the state it came from")
}
