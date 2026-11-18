package crdt_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// observerCalls counts the Observe and ObserveDeep calls on the three types of
// the chain root map "r" > map "mid" > nested type "inner".
type observerCalls struct {
	Inner     int `json:"inner"`
	InnerDeep int `json:"innerDeep"`
	Mid       int `json:"mid"`
	MidDeep   int `json:"midDeep"`
	Root      int `json:"root"`
	RootDeep  int `json:"rootDeep"`
}

// nestedKinds are the kinds of the type "inner" that a scenario writes into.
var nestedKinds = []string{"map", "array", "text"}

func nestedPrelim(kind string) any {
	switch kind {
	case "map":
		return crdt.NewMapPrelim()
	case "array":
		return crdt.NewArrayPrelim()
	default:
		return crdt.NewTextPrelim()
	}
}

// deletedTypeChain is one document's handles on the chain.
type deletedTypeChain struct {
	doc       *crdt.Doc
	root, mid *crdt.YMap
	inner     any
	calls     observerCalls
}

func newDeletedTypeChain(t *testing.T, doc *crdt.Doc) *deletedTypeChain {
	t.Helper()
	c := &deletedTypeChain{doc: doc, root: doc.GetMap("r")}
	v, ok := c.root.Get("mid")
	require.True(t, ok)
	c.mid = v.(*crdt.YMap)
	c.inner, ok = c.mid.Get("inner")
	require.True(t, ok)
	return c
}

// write makes one change in inner.
func (c *deletedTypeChain) write(txn *crdt.Transaction, v int) {
	switch in := c.inner.(type) {
	case *crdt.YMap:
		in.Set(txn, fmt.Sprint("k", v), v)
	case *crdt.YArray:
		in.Push(txn, []any{v})
	case *crdt.YText:
		in.Insert(txn, 0, fmt.Sprint(v), nil)
	}
}

// remove deletes "inner" from mid, or "mid" (and with it inner) from root.
func (c *deletedTypeChain) remove(txn *crdt.Transaction, target string) {
	if target == "inner" {
		c.mid.Delete(txn, "inner")
	} else {
		c.root.Delete(txn, "mid")
	}
}

func (c *deletedTypeChain) observe() {
	deep := func(n *int) func(*crdt.Transaction) { return func(*crdt.Transaction) { *n++ } }
	switch in := c.inner.(type) {
	case *crdt.YMap:
		in.Observe(func(crdt.YMapEvent) { c.calls.Inner++ })
	case *crdt.YArray:
		in.Observe(func(crdt.YArrayEvent) { c.calls.Inner++ })
	case *crdt.YText:
		in.Observe(func(crdt.YTextEvent) { c.calls.Inner++ })
	}
	c.inner.(interface {
		ObserveDeep(func(*crdt.Transaction)) func()
	}).ObserveDeep(deep(&c.calls.InnerDeep))
	c.mid.Observe(func(crdt.YMapEvent) { c.calls.Mid++ })
	c.mid.ObserveDeep(deep(&c.calls.MidDeep))
	c.root.Observe(func(crdt.YMapEvent) { c.calls.Root++ })
	c.root.ObserveDeep(deep(&c.calls.RootDeep))
}

// deletion is what a deletion of target calls in Yjs: Observe on the type that
// loses the key, and ObserveDeep on it and its live ancestors.
var deletion = map[string]observerCalls{
	"inner": {Mid: 1, MidDeep: 1, RootDeep: 1},
	"mid":   {Root: 1, RootDeep: 1},
}

func noCalls(string) observerCalls { return observerCalls{} }

func deletionCalls(target string) observerCalls { return deletion[target] }

// deletedTypeScenario runs on peer b of two synced peers a and b, and leaves in
// b.calls the observer calls for its last step. want is the Yjs result.
type deletedTypeScenario struct {
	name string
	run  func(t *testing.T, f updateFormat, a, b *deletedTypeChain, target string)
	want func(target string) observerCalls
}

// deletedTypeScenarios are the steps that the Yjs script in
// TestCompat_DeletedTypeObservers_MatchYjs also runs.
var deletedTypeScenarios = []deletedTypeScenario{
	{"remote write into a deleted type", func(t *testing.T, f updateFormat, a, b *deletedTypeChain, target string) {
		sv := b.doc.StateVector()
		a.doc.Transact(func(txn *crdt.Transaction) { a.write(txn, 1) })
		b.doc.Transact(func(txn *crdt.Transaction) { b.remove(txn, target) })
		b.calls = observerCalls{}
		require.NoError(t, f.apply(b.doc, f.encode(a.doc, sv), nil))
	}, noCalls},
	{"remote write and delete in one update", func(t *testing.T, f updateFormat, a, b *deletedTypeChain, target string) {
		sv := b.doc.StateVector()
		a.doc.Transact(func(txn *crdt.Transaction) { a.write(txn, 2); a.remove(txn, target) })
		require.NoError(t, f.apply(b.doc, f.encode(a.doc, sv), nil))
	}, deletionCalls},
	{"local write after delete", func(_ *testing.T, _ updateFormat, _, b *deletedTypeChain, target string) {
		b.doc.Transact(func(txn *crdt.Transaction) { b.remove(txn, target) })
		b.calls = observerCalls{}
		b.doc.Transact(func(txn *crdt.Transaction) { b.write(txn, 3) })
	}, noCalls},
	{"local write then delete in one transaction", func(_ *testing.T, _ updateFormat, _, b *deletedTypeChain, target string) {
		b.doc.Transact(func(txn *crdt.Transaction) { b.write(txn, 4); b.remove(txn, target) })
	}, deletionCalls},
	{"local delete then write in one transaction", func(_ *testing.T, _ updateFormat, _, b *deletedTypeChain, target string) {
		b.doc.Transact(func(txn *crdt.Transaction) { b.remove(txn, target); b.write(txn, 5) })
	}, deletionCalls},
	{"local delete of a type that has children", func(_ *testing.T, _ updateFormat, _, b *deletedTypeChain, target string) {
		b.doc.Transact(func(txn *crdt.Transaction) { b.write(txn, 7) })
		b.calls = observerCalls{}
		b.doc.Transact(func(txn *crdt.Transaction) { b.remove(txn, target) })
	}, deletionCalls},
	{"local write into a live type", func(_ *testing.T, _ updateFormat, _, b *deletedTypeChain, _ string) {
		b.doc.Transact(func(txn *crdt.Transaction) { b.write(txn, 6) })
	}, func(string) observerCalls { return observerCalls{Inner: 1, InnerDeep: 1, MidDeep: 1, RootDeep: 1} }},
}

// deletedTypeCase is one scenario with one update format, kind of inner,
// deleted type and GC setting.
type deletedTypeCase struct {
	name, kind, target string
	scenario           deletedTypeScenario
	f                  updateFormat
	gc                 bool
}

// deletedTypeCases lists every combination, each under its own name.
func deletedTypeCases() []deletedTypeCase {
	// Two deleted types and two GC settings per scenario, format and kind.
	cases := make([]deletedTypeCase, 0, 4*len(deletedTypeScenarios)*len(updateFormats)*len(nestedKinds))
	for _, sc := range deletedTypeScenarios {
		for _, f := range updateFormats {
			for _, kind := range nestedKinds {
				for _, target := range []string{"inner", "mid"} {
					for _, gc := range []bool{true, false} {
						cases = append(cases, deletedTypeCase{
							name: fmt.Sprintf("%s/%s/%s/delete %s/gc=%v", sc.name, f.name, kind, target, gc),
							kind: kind, target: target, scenario: sc, f: f, gc: gc,
						})
					}
				}
			}
		}
	}
	return cases
}

// run builds two synced peers holding r > mid > inner, with observers on b's
// three types, runs the scenario and returns b's observer calls.
func (c deletedTypeCase) run(t *testing.T) observerCalls {
	t.Helper()
	aDoc := crdt.New(crdt.WithClientID(1), crdt.WithGC(c.gc))
	bDoc := crdt.New(crdt.WithClientID(2), crdt.WithGC(c.gc))
	root := aDoc.GetMap("r")
	aDoc.Transact(func(txn *crdt.Transaction) {
		mid := crdt.NewMapPrelim()
		mid.Set(txn, "inner", nestedPrelim(c.kind))
		root.Set(txn, "mid", mid)
	})
	require.NoError(t, c.f.apply(bDoc, c.f.encode(aDoc, nil), nil))
	a, b := newDeletedTypeChain(t, aDoc), newDeletedTypeChain(t, bDoc)
	b.observe()
	c.scenario.run(t, c.f, a, b, c.target)
	return b.calls
}

// TestUnit_Doc_DeletedTypeFiresNoObservers checks that a write into a deleted
// type calls none of its observers and no deep observer of its ancestors, and
// that a deletion still fires the observers of the type it removed a key from,
// as in Yjs.
func TestUnit_Doc_DeletedTypeFiresNoObservers(t *testing.T) {
	for _, c := range deletedTypeCases() {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.scenario.want(c.target), c.run(t))
		})
	}
}

// TestCompat_DeletedTypeObservers_MatchYjs runs every scenario in Yjs and
// requires the same observer calls as ygo.
func TestCompat_DeletedTypeObservers_MatchYjs(t *testing.T) {
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	require.NoError(t, err)

	got := map[string]observerCalls{}
	for _, c := range deletedTypeCases() {
		got[c.name] = c.run(t)
	}

	script := `
const Y = require(process.argv[2]);
const formats = { V1: [Y.encodeStateAsUpdate, Y.applyUpdate], V2: [Y.encodeStateAsUpdateV2, Y.applyUpdateV2] };
const make = kind => kind === 'map' ? new Y.Map() : kind === 'array' ? new Y.Array() : new Y.Text();
const chain = doc => {
  const root = doc.getMap('r'), mid = root.get('mid');
  return { root, mid, inner: mid.get('inner') };
};
const write = (c, v) => {
  if (c.inner instanceof Y.Map) c.inner.set('k' + v, v);
  else if (c.inner instanceof Y.Array) c.inner.push([v]);
  else c.inner.insert(0, String(v));
};
const remove = (c, target) => target === 'inner' ? c.mid.delete('inner') : c.root.delete('mid');
const scenarios = {
  'remote write into a deleted type': (f, a, b, aDoc, bDoc, target, calls) => {
    const sv = Y.encodeStateVector(bDoc);
    aDoc.transact(() => write(a, 1));
    bDoc.transact(() => remove(b, target));
    for (const k in calls) calls[k] = 0;
    f[1](bDoc, f[0](aDoc, sv));
  },
  'remote write and delete in one update': (f, a, b, aDoc, bDoc, target) => {
    const sv = Y.encodeStateVector(bDoc);
    aDoc.transact(() => { write(a, 2); remove(a, target) });
    f[1](bDoc, f[0](aDoc, sv));
  },
  'local write after delete': (f, a, b, aDoc, bDoc, target, calls) => {
    bDoc.transact(() => remove(b, target));
    for (const k in calls) calls[k] = 0;
    bDoc.transact(() => write(b, 3));
  },
  'local write then delete in one transaction': (f, a, b, aDoc, bDoc, target) => bDoc.transact(() => { write(b, 4); remove(b, target) }),
  'local delete then write in one transaction': (f, a, b, aDoc, bDoc, target) => bDoc.transact(() => { remove(b, target); write(b, 5) }),
  'local delete of a type that has children': (f, a, b, aDoc, bDoc, target, calls) => {
    bDoc.transact(() => write(b, 7));
    for (const k in calls) calls[k] = 0;
    bDoc.transact(() => remove(b, target));
  },
  'local write into a live type': (f, a, b, aDoc, bDoc) => bDoc.transact(() => write(b, 6)),
};
const out = {};
for (const [name, run] of Object.entries(scenarios)) {
  for (const [fname, f] of Object.entries(formats)) {
    for (const kind of ['map', 'array', 'text']) {
      for (const target of ['inner', 'mid']) {
        for (const gc of [true, false]) {
          const aDoc = new Y.Doc({ gc }), bDoc = new Y.Doc({ gc });
          aDoc.clientID = 1; bDoc.clientID = 2;
          aDoc.transact(() => {
            const mid = new Y.Map();
            aDoc.getMap('r').set('mid', mid);
            mid.set('inner', make(kind));
          });
          f[1](bDoc, f[0](aDoc));
          const a = chain(aDoc), b = chain(bDoc);
          const calls = { inner: 0, innerDeep: 0, mid: 0, midDeep: 0, root: 0, rootDeep: 0 };
          b.inner.observe(() => calls.inner++); b.inner.observeDeep(() => calls.innerDeep++);
          b.mid.observe(() => calls.mid++); b.mid.observeDeep(() => calls.midDeep++);
          b.root.observe(() => calls.root++); b.root.observeDeep(() => calls.rootDeep++);
          run(f, a, b, aDoc, bDoc, target, calls);
          out[name + '/' + fname + '/' + kind + '/delete ' + target + '/gc=' + gc] = calls;
        }
      }
    }
  }
}
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "observers.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))
	out, err := exec.Command(nodePath, scriptPath, yjsPath).Output()
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
		t.Logf("node stderr:\n%s", exitErr.Stderr)
	}
	require.NoError(t, err)
	var want map[string]observerCalls
	require.NoError(t, json.Unmarshal(out, &want))
	require.Equal(t, want, got)
}
