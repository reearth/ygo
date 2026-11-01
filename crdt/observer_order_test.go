package crdt_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// orderTypes are one document's handles on root map "m", root text "t", root
// array "a", root map "r" > map "n" > text "x", and root maps "q0" to "q9".
type orderTypes struct {
	m, r, n *crdt.YMap
	t, x    *crdt.YText
	a       *crdt.YArray
	q       []*crdt.YMap
}

func newOrderTypes(t *testing.T, doc *crdt.Doc) orderTypes {
	t.Helper()
	o := orderTypes{m: doc.GetMap("m"), r: doc.GetMap("r"), t: doc.GetText("t"), a: doc.GetArray("a")}
	v, ok := o.r.Get("n")
	require.True(t, ok)
	o.n = v.(*crdt.YMap)
	v, ok = o.n.Get("x")
	require.True(t, ok)
	o.x = v.(*crdt.YText)
	for i := range orderRootCount {
		o.q = append(o.q, doc.GetMap(fmt.Sprint("q", i)))
	}
	return o
}

// observe makes each type record its name on got when its observer fires, and
// "deep <name>" when its deep observer fires.
func (o orderTypes) observe(got *[]string) {
	rec := func(name string) { *got = append(*got, name) }
	deep := func(name string) func(*crdt.Transaction) {
		return func(*crdt.Transaction) { rec("deep " + name) }
	}
	maps := map[string]*crdt.YMap{"m": o.m, "r": o.r, "n": o.n}
	for i, q := range o.q {
		maps[fmt.Sprint("q", i)] = q
	}
	for name, m := range maps {
		m.Observe(func(crdt.YMapEvent) { rec(name) })
		m.ObserveDeep(deep(name))
	}
	for name, txt := range map[string]*crdt.YText{"t": o.t, "x": o.x} {
		txt.Observe(func(crdt.YTextEvent) { rec(name) })
		txt.ObserveDeep(deep(name))
	}
	o.a.Observe(func(crdt.YArrayEvent) { rec("a") })
	o.a.ObserveDeep(deep("a"))
}

// orderScenario is one transaction. The Yjs script in
// TestCompat_ObserverOrder_MatchesYjs runs the same steps.
type orderScenario struct {
	name   string
	change func(txn *crdt.Transaction, o orderTypes)
	// local and remote are the observer calls in Yjs order; remote is the
	// same as local unless set.
	local, remote []string
}

var orderScenarios = []orderScenario{
	{"map then text", func(txn *crdt.Transaction, o orderTypes) {
		o.m.Set(txn, "k", 1)
		o.t.Insert(txn, 0, "z", nil)
	}, []string{"m", "t", "deep m", "deep t"}, nil},
	{"text then map", func(txn *crdt.Transaction, o orderTypes) {
		o.t.Insert(txn, 0, "z", nil)
		o.m.Set(txn, "k", 1)
	}, []string{"t", "m", "deep t", "deep m"}, nil},
	{"a type changed again keeps its place", func(txn *crdt.Transaction, o orderTypes) {
		o.a.Push(txn, []any{1})
		o.m.Set(txn, "k", 1)
		o.t.Insert(txn, 0, "z", nil)
		o.a.Push(txn, []any{2})
		o.m.Set(txn, "k", 2)
	}, []string{"a", "m", "t", "deep a", "deep m", "deep t"}, nil},
	{"nested text, array, then its root", func(txn *crdt.Transaction, o orderTypes) {
		o.x.Insert(txn, 0, "z", nil)
		o.a.Push(txn, []any{1})
		o.r.Set(txn, "k", 1)
	}, []string{"x", "a", "r", "deep x", "deep n", "deep r", "deep a"}, nil},
	{"root, then nested map and text", func(txn *crdt.Transaction, o orderTypes) {
		o.r.Set(txn, "k", 1)
		o.t.Insert(txn, 0, "z", nil)
		o.n.Set(txn, "k", 1)
		o.x.Insert(txn, 0, "z", nil)
	}, []string{"r", "t", "n", "x", "deep r", "deep t", "deep n", "deep x"}, nil},
	{"deletes in text, then map", func(txn *crdt.Transaction, o orderTypes) {
		o.t.Delete(txn, 0, 1)
		o.m.Delete(txn, "d")
	}, []string{"t", "m", "deep t", "deep m"},
		// A receiver applies an update's deletions in clock order, and the
		// map's entry is older than the text.
		[]string{"m", "t", "deep m", "deep t"}},
	{"more types than the commit orders inline", func(txn *crdt.Transaction, o orderTypes) {
		for i := len(o.q) - 1; i >= 0; i-- {
			o.q[i].Set(txn, "k", i)
		}
		o.m.Set(txn, "k", 1)
	}, manyRootsOrder(), nil},
}

// orderRootCount is the number of root maps "q0", "q1", ...: more types than
// the 8 the commit orders without allocating.
const orderRootCount = 10

// manyRootsOrder is the Yjs order for the scenario that changes the q maps
// from last to first, then m.
func manyRootsOrder() []string {
	var names []string
	for i := orderRootCount - 1; i >= 0; i-- {
		names = append(names, fmt.Sprint("q", i))
	}
	names = append(names, "m")
	order := slices.Clone(names)
	for _, name := range names {
		order = append(order, "deep "+name)
	}
	return order
}

// orderCase is one scenario, run locally or applied as a V1 or V2 update.
type orderCase struct {
	name     string
	scenario orderScenario
	f        *updateFormat
}

func orderCases() []orderCase {
	cases := make([]orderCase, 0, (1+len(updateFormats))*len(orderScenarios))
	for _, sc := range orderScenarios {
		cases = append(cases, orderCase{sc.name + "/local", sc, nil})
		for i := range updateFormats {
			cases = append(cases, orderCase{sc.name + "/remote " + updateFormats[i].name, sc, &updateFormats[i]})
		}
	}
	return cases
}

func (c orderCase) want() []string {
	if c.f != nil && c.scenario.remote != nil {
		return c.scenario.remote
	}
	return c.scenario.local
}

// run builds two synced peers, observes every type on b, runs the scenario on
// b (local) or on a and applies it to b (remote), and returns b's calls.
func (c orderCase) run(t *testing.T) []string {
	t.Helper()
	aDoc := crdt.New(crdt.WithClientID(1))
	bDoc := crdt.New(crdt.WithClientID(2))
	aDoc.Transact(func(txn *crdt.Transaction) {
		txn.GetMap("m").Set(txn, "d", 0)
		txn.GetText("t").Insert(txn, 0, "abc", nil)
		txn.GetArray("a")
		n := crdt.NewMapPrelim()
		n.Set(txn, "x", crdt.NewTextPrelim())
		txn.GetMap("r").Set(txn, "n", n)
	})
	require.NoError(t, crdt.ApplyUpdateV1(bDoc, crdt.EncodeStateAsUpdateV1(aDoc, nil), nil))
	a, b := newOrderTypes(t, aDoc), newOrderTypes(t, bDoc)
	var got []string
	b.observe(&got)
	if c.f == nil {
		bDoc.Transact(func(txn *crdt.Transaction) { c.scenario.change(txn, b) })
		return got
	}
	sv := bDoc.StateVector()
	aDoc.Transact(func(txn *crdt.Transaction) { c.scenario.change(txn, a) })
	require.NoError(t, c.f.apply(bDoc, c.f.encode(aDoc, sv), nil))
	return got
}

// TestUnit_Doc_ObserversFireInChangeOrder checks that per-type observers fire
// in the order the transaction first changed each type, and deep observers in
// the order Yjs collects them: each changed type, in that order, then its
// ancestors not yet named. Each case runs 20 times, since Go map order varies
// between runs.
func TestUnit_Doc_ObserversFireInChangeOrder(t *testing.T) {
	for _, c := range orderCases() {
		t.Run(c.name, func(t *testing.T) {
			for range 20 {
				require.Equal(t, c.want(), c.run(t))
			}
		})
	}
}

// TestCompat_ObserverOrder_MatchesYjs runs every case in Yjs and requires the
// same observer calls, in the same order, as ygo.
func TestCompat_ObserverOrder_MatchesYjs(t *testing.T) {
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	require.NoError(t, err)

	got := map[string][]string{}
	for _, c := range orderCases() {
		got[c.name] = c.run(t)
	}

	script := `
const Y = require(process.argv[2]);
const formats = { V1: [Y.encodeStateAsUpdate, Y.applyUpdate], V2: [Y.encodeStateAsUpdateV2, Y.applyUpdateV2] };
const qs = Array.from({ length: Number(process.argv[3]) }, (_, i) => 'q' + i);
const types = doc => {
  const m = doc.getMap('m'), t = doc.getText('t'), a = doc.getArray('a'), r = doc.getMap('r');
  const n = r.get('n');
  const o = { m, t, a, r, n, x: n.get('x') };
  for (const q of qs) o[q] = doc.getMap(q);
  return o;
};
const scenarios = {
  'map then text': o => { o.m.set('k', 1); o.t.insert(0, 'z') },
  'text then map': o => { o.t.insert(0, 'z'); o.m.set('k', 1) },
  'a type changed again keeps its place': o => {
    o.a.push([1]); o.m.set('k', 1); o.t.insert(0, 'z'); o.a.push([2]); o.m.set('k', 2);
  },
  'nested text, array, then its root': o => { o.x.insert(0, 'z'); o.a.push([1]); o.r.set('k', 1) },
  'root, then nested map and text': o => { o.r.set('k', 1); o.t.insert(0, 'z'); o.n.set('k', 1); o.x.insert(0, 'z') },
  'deletes in text, then map': o => { o.t.delete(0, 1); o.m.delete('d') },
  'more types than the commit orders inline': o => {
    for (let i = qs.length - 1; i >= 0; i--) o[qs[i]].set('k', i);
    o.m.set('k', 1);
  },
};
const out = {};
for (const [name, change] of Object.entries(scenarios)) {
  for (const mode of ['local', 'remote V1', 'remote V2']) {
    const aDoc = new Y.Doc(), bDoc = new Y.Doc();
    aDoc.clientID = 1; bDoc.clientID = 2;
    aDoc.transact(() => {
      aDoc.getMap('m').set('d', 0);
      aDoc.getText('t').insert(0, 'abc');
      aDoc.getArray('a');
      const n = new Y.Map();
      aDoc.getMap('r').set('n', n);
      n.set('x', new Y.Text());
    });
    Y.applyUpdate(bDoc, Y.encodeStateAsUpdate(aDoc));
    const a = types(aDoc), b = types(bDoc);
    const got = [];
    for (const name of ['m', 'r', 'n', 't', 'x', 'a', ...qs]) {
      b[name].observe(() => got.push(name));
      b[name].observeDeep(() => got.push('deep ' + name));
    }
    if (mode === 'local') {
      bDoc.transact(() => change(b));
    } else {
      const f = formats[mode.slice(7)];
      const sv = Y.encodeStateVector(bDoc);
      aDoc.transact(() => change(a));
      f[1](bDoc, f[0](aDoc, sv));
    }
    out[name + '/' + mode] = got;
  }
}
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "order.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))
	out, err := exec.Command(nodePath, scriptPath, yjsPath, fmt.Sprint(orderRootCount)).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Logf("node stderr:\n%s", exitErr.Stderr)
	}
	require.NoError(t, err)
	var want map[string][]string
	require.NoError(t, json.Unmarshal(out, &want))
	require.Equal(t, want, got)
}
