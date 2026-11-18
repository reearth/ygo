package crdt_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// undoDiffSteps is the number of operations in one differential session.
const undoDiffSteps = 150

// knownUndoDivergences lists the sessions, among the first 1,500, whose
// content still differs from Yjs, with the first step that differs. A change
// that fixes one fails the test until its entry is removed here.
var knownUndoDivergences = map[uint32]int{}

// TestDifferential_UndoMatchesYjs drives one operation stream through Yjs and
// ygo and compares the root array's JSON after every step: top-level array
// inserts and deletes, edits of nested arrays, one-character inserts into
// nested texts, undo and redo. Both sides draw every choice from the same
// xorshift32 stream. It runs only when YGO_UNDO_DIFF_SEEDS names a number of
// sessions, for example YGO_UNDO_DIFF_SEEDS=1500.
func TestDifferential_UndoMatchesYjs(t *testing.T) {
	v := os.Getenv("YGO_UNDO_DIFF_SEEDS")
	if v == "" {
		t.Skip("YGO_UNDO_DIFF_SEEDS not set")
	}
	seeds, err := strconv.Atoi(v)
	if err != nil || seeds <= 0 {
		t.Fatalf("YGO_UNDO_DIFF_SEEDS=%q is not a positive number", v)
	}
	nodePath, testutilDir := requireConformance(t, "yjs")
	script := filepath.Join(t.TempDir(), "undo_diff.js")
	if err := os.WriteFile(script, []byte(undoDiffScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nodePath, script, filepath.Join(testutilDir, "node_modules", "yjs"), strconv.Itoa(seeds), strconv.Itoa(undoDiffSteps))
	cmd.Dir = testutilDir
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	yjs := bufio.NewScanner(out)
	yjs.Buffer(make([]byte, 1<<20), 1<<24)

	diverged := 0
	for seed := range uint32(seeds) {
		got := undoDiffSession(t, seed)
		first := -1
		for step := range undoDiffSteps {
			if !yjs.Scan() {
				t.Fatalf("node stopped at seed %d step %d: %v", seed, step, yjs.Err())
			}
			if first < 0 && !sameJSON(got[step], yjs.Text()) {
				first = step
				kind := divergenceKind(got[step], yjs.Text())
				if _, known := knownUndoDivergences[seed]; !known {
					t.Errorf("seed %d step %d (%s): ygo %s, Yjs %s", seed, step, kind, got[step], yjs.Text())
				} else {
					t.Logf("known: seed %d step %d (%s)", seed, step, kind)
				}
			}
		}
		if first >= 0 {
			diverged++
		}
		if want, known := knownUndoDivergences[seed]; known && first != want {
			t.Errorf("seed %d: first divergence at step %d, known at step %d; update knownUndoDivergences", seed, first, want)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d of %d sessions differ from Yjs", diverged, seeds)
}

func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return a == b
	}
	xs, _ := json.Marshal(x)
	ys, _ := json.Marshal(y)
	return string(xs) == string(ys)
}

// divergenceKind says how ygo's content differs from Yjs's: "extra" when it
// holds every value Yjs holds and more, "order" when it holds the same values
// in another order, and "missing" when it lacks a value Yjs holds.
func divergenceKind(ygo, yjs string) string {
	count := func(s string) map[string]int {
		var v any
		_ = json.Unmarshal([]byte(s), &v)
		n := map[string]int{}
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case []any:
				for _, e := range x {
					walk(e)
				}
			case string:
				for _, r := range x {
					n["s"+string(r)]++
				}
			default:
				b, _ := json.Marshal(x)
				n[string(b)]++
			}
		}
		walk(v)
		return n
	}
	g, y := count(ygo), count(yjs)
	extra := false
	for k, c := range y {
		if g[k] < c {
			return "missing"
		}
		if g[k] > c {
			extra = true
		}
	}
	for k, c := range g {
		if y[k] == 0 && c > 0 {
			extra = true
		}
	}
	if extra {
		return "extra"
	}
	return "order"
}

// undoDiffRand is xorshift32, as in undoDiffScript.
type undoDiffRand uint32

func (r *undoDiffRand) next() int {
	x := uint32(*r)
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	*r = undoDiffRand(x)
	return int(x)
}

// undoDiffSession runs one session in ygo and returns the root array's JSON
// after each step.
func undoDiffSession(t *testing.T, seed uint32) []string {
	r := undoDiffRand(seed*2654435761 + 1)
	if r == 0 {
		r = 1
	}
	doc := crdt.New(crdt.WithClientID(1))
	arr := doc.GetArray("a")
	um := crdt.NewUndoManager(doc, []crdt.SharedType{arr})
	defer um.Destroy()
	kinds := func(want func(any) bool) []any {
		var out []any
		for i := range arr.Len() {
			if v := arr.Get(i); want(v) {
				out = append(out, v)
			}
		}
		return out
	}
	steps := make([]string, undoDiffSteps)
	for step := range steps {
		switch op := r.next() % 10; {
		case op < 3:
			pos := r.next() % (arr.Len() + 1)
			switch r.next() % 3 {
			case 0:
				n := r.next() % 100
				doc.Transact(func(txn *crdt.Transaction) { arr.Insert(txn, pos, []any{n}) })
			case 1:
				doc.Transact(func(txn *crdt.Transaction) { arr.InsertType(txn, pos, crdt.NewArrayPrelim()) })
			default:
				doc.Transact(func(txn *crdt.Transaction) { arr.InsertType(txn, pos, crdt.NewTextPrelim()) })
			}
		case op < 4:
			if n := arr.Len(); n > 0 {
				pos := r.next() % n
				doc.Transact(func(txn *crdt.Transaction) { arr.Delete(txn, pos, 1) })
			}
		case op < 6:
			if nested := kinds(func(v any) bool { _, ok := v.(*crdt.YArray); return ok }); len(nested) > 0 {
				a := nested[r.next()%len(nested)].(*crdt.YArray)
				n := a.Len()
				if c := r.next() % 3; c != 0 || n == 0 {
					pos, v := r.next()%(n+1), r.next()%100
					doc.Transact(func(txn *crdt.Transaction) { a.Insert(txn, pos, []any{v}) })
				} else {
					pos := r.next() % n
					doc.Transact(func(txn *crdt.Transaction) { a.Delete(txn, pos, 1) })
				}
			}
		case op < 7:
			if texts := kinds(func(v any) bool { _, ok := v.(*crdt.YText); return ok }); len(texts) > 0 {
				txt := texts[r.next()%len(texts)].(*crdt.YText)
				pos, ch := r.next()%(txt.Len()+1), string(rune('a'+r.next()%26))
				doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, pos, ch, nil) })
			}
		case op < 9:
			um.Undo()
		default:
			um.Redo()
		}
		um.StopCapturing()
		js, err := arr.ToJSON()
		if err != nil {
			t.Fatal(fmt.Errorf("seed %d step %d: %w", seed, step, err))
		}
		steps[step] = string(js)
	}
	return steps
}

// undoDiffScript is undoDiffSession in Yjs: it prints the root array's JSON
// after each step, one line per step, for seeds 0 to argv[3]-1.
const undoDiffScript = `
const Y = require(process.argv[2])
const [seeds, steps] = process.argv.slice(3).map(Number)
const lines = []
for (let seed = 0; seed < seeds; seed++) {
  let x = (Math.imul(seed, 2654435761) + 1) >>> 0
  if (x === 0) x = 1
  const next = () => {
    x = (x ^ (x << 13)) >>> 0
    x = (x ^ (x >>> 17)) >>> 0
    x = (x ^ (x << 5)) >>> 0
    return x
  }
  const doc = new Y.Doc()
  doc.clientID = 1
  const arr = doc.getArray('a')
  const um = new Y.UndoManager(arr)
  const kinds = (want) => arr.toArray().filter(want)
  for (let step = 0; step < steps; step++) {
    const op = next() % 10
    if (op < 3) {
      const pos = next() % (arr.length + 1)
      const kind = next() % 3
      if (kind === 0) arr.insert(pos, [next() % 100])
      else if (kind === 1) arr.insert(pos, [new Y.Array()])
      else arr.insert(pos, [new Y.Text()])
    } else if (op < 4) {
      if (arr.length > 0) arr.delete(next() % arr.length, 1)
    } else if (op < 6) {
      const nested = kinds((v) => v instanceof Y.Array)
      if (nested.length > 0) {
        const a = nested[next() % nested.length]
        const n = a.length
        const c = next() % 3
        if (c !== 0 || n === 0) {
          const pos = next() % (n + 1)
          a.insert(pos, [next() % 100])
        } else {
          a.delete(next() % n, 1)
        }
      }
    } else if (op < 7) {
      const texts = kinds((v) => v instanceof Y.Text)
      if (texts.length > 0) {
        const t = texts[next() % texts.length]
        const pos = next() % (t.length + 1)
        t.insert(pos, String.fromCharCode(97 + next() % 26))
      }
    } else if (op < 9) {
      um.undo()
    } else {
      um.redo()
    }
    um.stopCapturing()
    lines.push(JSON.stringify(arr.toJSON()))
  }
  if (lines.length >= 4096 || seed === seeds - 1) {
    process.stdout.write(lines.join('\n') + '\n')
    lines.length = 0
  }
}
`
