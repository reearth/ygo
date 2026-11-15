package crdt_test

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// editOp is one transaction of an editing session: a text insert or delete,
// a map set, an array insert or delete, or two text deletions (B, then the
// earlier A).
type editOp struct {
	Op  string `json:"op"`
	Pos int    `json:"pos"`
	Len int    `json:"len"`
	Key string `json:"key"`
	Val int    `json:"val"`
	A   int    `json:"a"`
	B   int    `json:"b"`
}

// editingSession returns n transactions of typing with deletions, map
// overwrites and array edits, the same for every seed.
func editingSession(n int, seed uint64) []editOp {
	r := rand.New(rand.NewPCG(seed, 0))
	var ops []editOp
	textLen, arrLen, cursor := 0, 0, 0
	for len(ops) < n {
		switch p := r.IntN(100); {
		case p < 60:
			if r.IntN(20) == 0 {
				cursor = r.IntN(textLen + 1)
			}
			ops = append(ops, editOp{Op: "ins", Pos: cursor})
			textLen++
			cursor++
		case p < 85 && textLen > 0:
			k := min(1+r.IntN(4), cursor, textLen)
			if k == 0 {
				cursor = textLen
				continue
			}
			cursor -= k
			ops = append(ops, editOp{Op: "del", Pos: cursor, Len: k})
			textLen -= k
		case p < 92:
			ops = append(ops, editOp{Op: "mset", Key: string(rune('a' + r.IntN(8))), Val: r.IntN(1000)})
		case p < 97:
			ops = append(ops, editOp{Op: "ains", Pos: r.IntN(arrLen + 1), Val: r.IntN(1000)})
			arrLen++
		case p < 99 && arrLen > 0:
			ops = append(ops, editOp{Op: "adel", Pos: r.IntN(arrLen)})
			arrLen--
		case textLen >= 10:
			a := r.IntN(textLen - 6)
			ops = append(ops, editOp{Op: "multi", A: a, B: a + 3 + r.IntN(textLen-a-4)})
			textLen -= 2
			cursor = min(cursor, textLen)
		}
	}
	return ops
}

// TestCompat_OnUpdate_MatchesYjsUpdateEvents replays one editing session,
// one transaction per op, through ygo and through Yjs, and requires each
// OnUpdate payload to equal the bytes of Yjs's update event.
func TestCompat_OnUpdate_MatchesYjsUpdateEvents(t *testing.T) {
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	require.NoError(t, err)

	ops := editingSession(1500, 7)
	doc := crdt.New(crdt.WithClientID(1))
	txt, m, arr := doc.GetText("t"), doc.GetMap("m"), doc.GetArray("a")
	var got [][]byte
	doc.OnUpdate(func(u []byte, _ any) { got = append(got, u) })
	for _, op := range ops {
		doc.Transact(func(txn *crdt.Transaction) {
			switch op.Op {
			case "ins":
				txt.Insert(txn, op.Pos, "x", nil)
			case "del":
				txt.Delete(txn, op.Pos, op.Len)
			case "mset":
				m.Set(txn, op.Key, op.Val)
			case "ains":
				arr.Insert(txn, op.Pos, []any{op.Val})
			case "adel":
				arr.Delete(txn, op.Pos, 1)
			case "multi":
				txt.Delete(txn, op.B, 1)
				txt.Delete(txn, op.A, 1)
			}
		})
	}

	dir := t.TempDir()
	input, err := json.Marshal(ops)
	require.NoError(t, err)
	inputPath := filepath.Join(dir, "ops.json")
	require.NoError(t, os.WriteFile(inputPath, input, 0o644))
	script := `
const Y = require(process.argv[2]);
const ops = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const doc = new Y.Doc();
doc.clientID = 1;
const txt = doc.getText('t'), m = doc.getMap('m'), arr = doc.getArray('a');
const out = [];
doc.on('update', u => out.push(Buffer.from(u).toString('base64')));
for (const op of ops) {
  doc.transact(() => {
    switch (op.op) {
      case 'ins': txt.insert(op.pos, 'x'); break;
      case 'del': txt.delete(op.pos, op.len); break;
      case 'mset': m.set(op.key, op.val); break;
      case 'ains': arr.insert(op.pos, [op.val]); break;
      case 'adel': arr.delete(op.pos, 1); break;
      case 'multi': txt.delete(op.b, 1); txt.delete(op.a, 1); break;
    }
  });
}
process.stdout.write(JSON.stringify(out));
`
	scriptPath := filepath.Join(dir, "replay.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))
	out, err := exec.Command(nodePath, scriptPath, yjsPath, inputPath).Output()
	require.NoError(t, err)
	var want [][]byte
	require.NoError(t, json.Unmarshal(out, &want))

	require.Len(t, got, len(want))
	for i := range want {
		require.Equalf(t, want[i], got[i], "update of transaction %d (%+v)", i, ops[i])
	}
}
