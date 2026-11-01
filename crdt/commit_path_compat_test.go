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

// commitPathScenario is one step on document b, with the observers
// observeCommitPath registers on b; a is a second peer that sends b updates.
// The Yjs script in TestCompat_CommitPath_MatchesYjs runs the same steps.
type commitPathScenario struct {
	name string
	// throws names the observers of b that panic with "boom".
	throws []string
	// before runs before b's observers are registered.
	before func(a, b *crdt.Doc)
	run    func(a, b *crdt.Doc)
}

// sendCommitPath makes change on a and applies to b, with origin "remote",
// the update of what b lacks.
func sendCommitPath(a, b *crdt.Doc, change func(txn *crdt.Transaction)) {
	sv := b.StateVector()
	a.Transact(change)
	if err := crdt.ApplyUpdateV1(b, crdt.EncodeStateAsUpdateV1(a, sv), "remote"); err != nil {
		panic(err)
	}
}

func insertAndDelete(txn *crdt.Transaction) {
	txt := txn.GetText("t")
	txt.Insert(txn, 0, "abc", nil)
	txt.Delete(txn, 1, 1)
}

// deletionHistory types "abcdef" and then deletes "b" and "d", each in its
// own transaction.
func deletionHistory(doc *crdt.Doc) {
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "abcdef", nil) })
	doc.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 1, 1) })
	doc.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 2, 1) })
}

// boldBeforeRemoteDelete gives b the text "abc" from a and bolds "c" on b;
// the scenario then deletes "abc" on a, which leaves b's markers redundant.
func boldBeforeRemoteDelete(a, b *crdt.Doc) {
	sendCommitPath(a, b, func(txn *crdt.Transaction) { txn.GetText("t").Insert(txn, 0, "abc", nil) })
	txt := b.GetText("t")
	b.Transact(func(txn *crdt.Transaction) { txt.Format(txn, 2, 1, crdt.Attributes{"bold": true}) })
}

func deleteAll(txn *crdt.Transaction) { txn.GetText("t").Delete(txn, 0, 3) }

func textAndMap(txn *crdt.Transaction) {
	txn.GetText("t").Insert(txn, 0, "a", nil)
	txn.GetMap("m").Set(txn, "k", 1)
}

var commitPathScenarios = []commitPathScenario{
	{name: "local insert and delete in one transaction", run: func(_, b *crdt.Doc) { b.Transact(insertAndDelete) }},
	{name: "remote insert and delete in one transaction", run: func(a, b *crdt.Doc) { sendCommitPath(a, b, insertAndDelete) }},
	{name: "local transaction that changes nothing", run: func(_, b *crdt.Doc) { b.Transact(func(*crdt.Transaction) {}) }},
	{
		name: "remote update the document holds",
		before: func(a, b *crdt.Doc) {
			sendCommitPath(a, b, func(txn *crdt.Transaction) { txn.GetText("t").Insert(txn, 0, "abc", nil) })
		},
		run: func(a, b *crdt.Doc) {
			if err := crdt.ApplyUpdateV1(b, crdt.EncodeStateAsUpdateV1(a, nil), "remote"); err != nil {
				panic(err)
			}
		},
	},
	{
		name:   "local keystroke after deletions",
		before: func(_, b *crdt.Doc) { deletionHistory(b) },
		run: func(_, b *crdt.Doc) {
			b.Transact(func(txn *crdt.Transaction) { txn.GetText("t").Insert(txn, 4, "x", nil) })
		},
	},
	{
		name:   "local deletion after deletions",
		before: func(_, b *crdt.Doc) { deletionHistory(b) },
		run: func(_, b *crdt.Doc) {
			b.Transact(func(txn *crdt.Transaction) { txn.GetText("t").Delete(txn, 0, 1) })
		},
	},
	{
		name: "remote deletion after deletions",
		before: func(a, b *crdt.Doc) {
			deletionHistory(a)
			if err := crdt.ApplyUpdateV1(b, crdt.EncodeStateAsUpdateV1(a, nil), nil); err != nil {
				panic(err)
			}
		},
		run: func(a, b *crdt.Doc) {
			sendCommitPath(a, b, func(txn *crdt.Transaction) { txn.GetText("t").Delete(txn, 0, 1) })
		},
	},
	{
		name: "remote delete that parks, then its insert",
		run: func(a, b *crdt.Doc) {
			txt := a.GetText("t")
			sv0 := a.StateVector()
			a.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "xy", nil) })
			insert := crdt.EncodeStateAsUpdateV1(a, sv0)
			sv1 := a.StateVector()
			a.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 0, 1) })
			for _, u := range [][]byte{crdt.EncodeStateAsUpdateV1(a, sv1), insert} {
				if err := crdt.ApplyUpdateV1(b, u, "remote"); err != nil {
					panic(err)
				}
			}
		},
	},
	{
		name: "local map set and subdocument",
		run: func(_, b *crdt.Doc) {
			b.Transact(func(txn *crdt.Transaction) {
				m := txn.GetMap("m")
				m.Set(txn, "k", 1)
				m.Set(txn, "sub", crdt.New(crdt.WithGUID("sub"), crdt.WithGC(false)))
			})
		},
	},
	{name: "first text observer throws", throws: []string{"observe t 1"}, run: func(_, b *crdt.Doc) { b.Transact(textAndMap) }},
	{name: "deep observer throws", throws: []string{"deep t"}, run: func(_, b *crdt.Doc) { b.Transact(textAndMap) }},
	{name: "afterTransaction throws", throws: []string{"afterTransaction"}, run: func(_, b *crdt.Doc) { b.Transact(textAndMap) }},
	{
		name:   "remote change that leaves markers redundant",
		before: boldBeforeRemoteDelete,
		run:    func(a, b *crdt.Doc) { sendCommitPath(a, b, deleteAll) },
	},
	{
		name:   "remote change that leaves markers redundant, text observer throws",
		throws: []string{"observe t 1"},
		before: boldBeforeRemoteDelete,
		run:    func(a, b *crdt.Doc) { sendCommitPath(a, b, deleteAll) },
	},
}

// observeCommitPath registers observers on doc that append their calls to got,
// as the Yjs script names them, and panic with "boom" when throws names them.
func observeCommitPath(doc *crdt.Doc, throws []string, got *[]string) {
	rec := func(name string) {
		*got = append(*got, name)
		if slices.Contains(throws, name) {
			panic("boom")
		}
	}
	txt, m := doc.GetText("t"), doc.GetMap("m")
	txt.Observe(func(crdt.YTextEvent) { rec("observe t 1") })
	txt.Observe(func(crdt.YTextEvent) { rec("observe t 2") })
	txt.ObserveDeep(func(*crdt.Transaction) { rec("deep t") })
	m.Observe(func(crdt.YMapEvent) { rec("observe m") })
	m.ObserveDeep(func(*crdt.Transaction) { rec("deep m") })
	doc.OnAfterTransaction(func(*crdt.Transaction) { rec("afterTransaction") })
	doc.OnUpdate(func(u []byte, origin any) {
		if origin == nil {
			origin = "null"
		}
		rec(fmt.Sprintf("update %v %x", origin, u))
	})
	doc.OnSubdocs(func(crdt.SubdocsEvent) { rec("subdocs") })
}

// calls returns b's observer calls for the scenario, then "caught" and the
// panic, if any, that reached the caller.
func (sc commitPathScenario) calls() (got []string) {
	a := crdt.New(crdt.WithClientID(1))
	b := crdt.New(crdt.WithClientID(2))
	if sc.before != nil {
		sc.before(a, b)
	}
	observeCommitPath(b, sc.throws, &got)
	defer func() {
		if p := recover(); p != nil {
			got = append(got, fmt.Sprint("caught ", p))
		}
	}()
	sc.run(a, b)
	return got
}

// TestCompat_CommitPath_MatchesYjs runs each scenario in ygo and in Yjs and
// requires the same observer calls in the same order, the same OnUpdate
// bytes and origins, and the same panic: which hooks a commit calls, in which
// order, what the update carries, and that a throwing observer stops none of
// the others.
func TestCompat_CommitPath_MatchesYjs(t *testing.T) {
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	require.NoError(t, err)

	got := map[string][]string{}
	for _, sc := range commitPathScenarios {
		got[sc.name] = sc.calls()
	}

	script := `
const Y = require(process.argv[2]);
const send = (a, b, change) => {
  const sv = Y.encodeStateVector(b);
  a.transact(() => change(a));
  Y.applyUpdate(b, Y.encodeStateAsUpdate(a, sv), 'remote');
};
const insertAndDelete = d => { const t = d.getText('t'); t.insert(0, 'abc'); t.delete(1, 1); };
const deletionHistory = d => {
  const t = d.getText('t');
  d.transact(() => t.insert(0, 'abcdef'));
  d.transact(() => t.delete(1, 1));
  d.transact(() => t.delete(2, 1));
};
const boldBeforeRemoteDelete = (a, b) => {
  send(a, b, d => d.getText('t').insert(0, 'abc'));
  b.transact(() => b.getText('t').format(2, 1, { bold: true }));
};
const deleteAll = d => d.getText('t').delete(0, 3);
const textAndMap = d => { d.getText('t').insert(0, 'a'); d.getMap('m').set('k', 1); };
const scenarios = [
  { name: 'local insert and delete in one transaction', run: (a, b) => b.transact(() => insertAndDelete(b)) },
  { name: 'remote insert and delete in one transaction', run: (a, b) => send(a, b, insertAndDelete) },
  { name: 'local transaction that changes nothing', run: (a, b) => b.transact(() => {}) },
  {
    name: 'remote update the document holds',
    before: (a, b) => send(a, b, d => d.getText('t').insert(0, 'abc')),
    run: (a, b) => Y.applyUpdate(b, Y.encodeStateAsUpdate(a), 'remote'),
  },
  { name: 'local keystroke after deletions', before: (a, b) => deletionHistory(b), run: (a, b) => b.transact(() => b.getText('t').insert(4, 'x')) },
  { name: 'local deletion after deletions', before: (a, b) => deletionHistory(b), run: (a, b) => b.transact(() => b.getText('t').delete(0, 1)) },
  {
    name: 'remote deletion after deletions',
    before: (a, b) => { deletionHistory(a); Y.applyUpdate(b, Y.encodeStateAsUpdate(a)); },
    run: (a, b) => send(a, b, d => d.getText('t').delete(0, 1)),
  },
  {
    name: 'remote delete that parks, then its insert',
    run: (a, b) => {
      const t = a.getText('t');
      const sv0 = Y.encodeStateVector(a);
      a.transact(() => t.insert(0, 'xy'));
      const insert = Y.encodeStateAsUpdate(a, sv0);
      const sv1 = Y.encodeStateVector(a);
      a.transact(() => t.delete(0, 1));
      for (const u of [Y.encodeStateAsUpdate(a, sv1), insert]) Y.applyUpdate(b, u, 'remote');
    },
  },
  {
    name: 'local map set and subdocument',
    run: (a, b) => b.transact(() => { const m = b.getMap('m'); m.set('k', 1); m.set('sub', new Y.Doc({ guid: 'sub', gc: false })); }),
  },
  { name: 'first text observer throws', throws: ['observe t 1'], run: (a, b) => b.transact(() => textAndMap(b)) },
  { name: 'deep observer throws', throws: ['deep t'], run: (a, b) => b.transact(() => textAndMap(b)) },
  { name: 'afterTransaction throws', throws: ['afterTransaction'], run: (a, b) => b.transact(() => textAndMap(b)) },
  { name: 'remote change that leaves markers redundant', before: boldBeforeRemoteDelete, run: (a, b) => send(a, b, deleteAll) },
  {
    name: 'remote change that leaves markers redundant, text observer throws',
    throws: ['observe t 1'],
    before: boldBeforeRemoteDelete,
    run: (a, b) => send(a, b, deleteAll),
  },
];
const out = {};
for (const sc of scenarios) {
  const a = new Y.Doc(), b = new Y.Doc();
  a.clientID = 1; b.clientID = 2;
  if (sc.before) sc.before(a, b);
  const got = [];
  const throws = sc.throws || [];
  const rec = name => { got.push(name); if (throws.includes(name)) throw 'boom'; };
  const t = b.getText('t'), m = b.getMap('m');
  t.observe(() => rec('observe t 1'));
  t.observe(() => rec('observe t 2'));
  t.observeDeep(() => rec('deep t'));
  m.observe(() => rec('observe m'));
  m.observeDeep(() => rec('deep m'));
  b.on('afterTransaction', () => rec('afterTransaction'));
  b.on('update', (u, origin) => rec('update ' + (origin === null ? 'null' : origin) + ' ' + Buffer.from(u).toString('hex')));
  b.on('subdocs', () => rec('subdocs'));
  try {
    sc.run(a, b);
  } catch (e) {
    got.push('caught ' + e);
  }
  out[sc.name] = got;
}
process.stdout.write(JSON.stringify(out));
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "commit_path.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))
	out, err := exec.Command(nodePath, scriptPath, yjsPath).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Logf("node stderr:\n%s", exitErr.Stderr)
	}
	require.NoError(t, err)
	var want map[string][]string
	require.NoError(t, json.Unmarshal(out, &want))
	require.Len(t, want, len(commitPathScenarios))
	for _, sc := range commitPathScenarios {
		t.Run(sc.name, func(t *testing.T) { require.Equal(t, want[sc.name], got[sc.name]) })
	}
}
