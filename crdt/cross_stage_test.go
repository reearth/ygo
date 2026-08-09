package crdt

import (
	"strings"
	"testing"
)

// #222: staging one detached handle onto two DIFFERENT containers must fail at
// the second staging call — not later, at attach, inside flushPrelim, with a
// panic naming a function the caller never used. The same-container duplicate
// is already rejected at the call (#213's rejectAlreadyStaged); these tests
// close the cross-container half.

// mustPanicContaining runs fn and requires it to panic with a message
// containing want. It fails the test if fn returns normally.
func mustPanicContaining(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("want panic containing %q, got no panic", want)
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, want) {
			t.Fatalf("want panic containing %q, got %v", want, r)
		}
	}()
	fn()
}

// Staging the same detached handle onto two different DETACHED containers must
// panic at the second call, naming the entry point actually used.
func TestUnit_CrossStage_TwoDetachedContainers_PanicsAtSecondCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantFn  string
		staleFn func(txn *Transaction, m *YMap) // first staging, container 1
		dupFn   func(txn *Transaction, m *YMap) // second staging, container 2 — must panic
	}{
		{
			"array PushType then array PushType",
			"PushType",
			func(txn *Transaction, m *YMap) { NewArrayPrelim().PushType(txn, m) },
			func(txn *Transaction, m *YMap) { NewArrayPrelim().PushType(txn, m) },
		},
		{
			"array PushType then array InsertType",
			"InsertType",
			func(txn *Transaction, m *YMap) { NewArrayPrelim().PushType(txn, m) },
			func(txn *Transaction, m *YMap) { NewArrayPrelim().InsertType(txn, 0, m) },
		},
		{
			"array PushType then map Set",
			"Set",
			func(txn *Transaction, m *YMap) { NewArrayPrelim().PushType(txn, m) },
			func(txn *Transaction, m *YMap) { NewMapPrelim().Set(txn, "k", m) },
		},
		{
			"map Set then array PushType",
			"PushType",
			func(txn *Transaction, m *YMap) { NewMapPrelim().Set(txn, "k", m) },
			func(txn *Transaction, m *YMap) { NewArrayPrelim().PushType(txn, m) },
		},
		{
			"map Set then map Set",
			"Set",
			func(txn *Transaction, m *YMap) { NewMapPrelim().Set(txn, "k", m) },
			func(txn *Transaction, m *YMap) { NewMapPrelim().Set(txn, "k", m) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			m := NewMapPrelim()
			doc.Transact(func(txn *Transaction) {
				tc.staleFn(txn, m)
				mustPanicContaining(t, tc.wantFn, func() { tc.dupFn(txn, m) })
			})
		})
	}
}

// Staging on a detached container and then handing the same handle to an
// ATTACHED container must also panic at the call: the handle is still
// detached (staged ≠ attached), so the pre-#222 entry-point check let it
// integrate — and the detached container's later attach then blew up inside
// flushPrelim blaming PushType.
func TestUnit_CrossStage_StagedThenAttachedContainer_PanicsAtCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wantFn string
		dupFn  func(txn *Transaction, doc *Doc, m *YMap)
	}{
		{"attached PushType", "PushType",
			func(txn *Transaction, doc *Doc, m *YMap) { txn.GetArray("b").PushType(txn, m) }},
		{"attached InsertType", "InsertType",
			func(txn *Transaction, doc *Doc, m *YMap) { txn.GetArray("b").InsertType(txn, 0, m) }},
		{"attached map Set", "Set",
			func(txn *Transaction, doc *Doc, m *YMap) { txn.GetMap("bm").Set(txn, "k", m) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			staging := NewArrayPrelim()
			m := NewMapPrelim()
			doc.Transact(func(txn *Transaction) {
				staging.PushType(txn, m) // staged on a detached array
				mustPanicContaining(t, tc.wantFn, func() { tc.dupFn(txn, doc, m) })
			})
		})
	}
}

// One handle under two different KEYS of the same detached map is the same
// attach-twice bug inside a single container, and must fail at the second Set.
func TestUnit_CrossStage_SameMapTwoKeys_PanicsAtSecondSet(t *testing.T) {
	doc := newTestDoc(1)
	outer := NewMapPrelim()
	inner := NewTextPrelim()
	doc.Transact(func(txn *Transaction) {
		outer.Set(txn, "a", inner)
		mustPanicContaining(t, "Set", func() { outer.Set(txn, "b", inner) })
	})
}

// Displacement releases the handle: a staged type removed from its container
// (map overwrite, map delete, array delete) is legitimately re-stageable, and
// the whole build must attach cleanly afterwards.
func TestUnit_CrossStage_DisplacedHandleIsRestageable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		displace func(txn *Transaction, arr *YArray, mp *YMap, h *YText)
	}{
		{"map overwrite", func(txn *Transaction, _ *YArray, mp *YMap, h *YText) {
			mp.Set(txn, "k", h)
			mp.Set(txn, "k", "plain instead")
		}},
		{"map delete", func(txn *Transaction, _ *YArray, mp *YMap, h *YText) {
			mp.Set(txn, "k", h)
			mp.Delete(txn, "k")
		}},
		{"array delete", func(txn *Transaction, arr *YArray, _ *YMap, h *YText) {
			arr.Push(txn, []any{"x"})
			arr.PushType(txn, h)
			arr.Delete(txn, 1, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			root := doc.GetMap("root")
			arr := NewArrayPrelim()
			mp := NewMapPrelim()
			h := NewTextPrelim()

			doc.Transact(func(txn *Transaction) {
				tc.displace(txn, arr, mp, h)
				h.Insert(txn, 0, "hello", nil)

				// h left its first container, so a second container may take it.
				dest := NewMapPrelim()
				dest.Set(txn, "text", h)
				root.Set(txn, "dest", dest)
				root.Set(txn, "arr", arr)
				root.Set(txn, "mp", mp)
			})

			dest, _ := root.Get("dest")
			dm, ok := dest.(*YMap)
			if !ok {
				t.Fatalf("dest = %T, want *YMap", dest)
			}
			got, _ := dm.Get("text")
			txt, ok := got.(*YText)
			if !ok || txt.ToString() != "hello" {
				t.Fatalf("dest.text = %v, want the re-staged YText reading %q", got, "hello")
			}
		})
	}
}

// Overwriting a staged key with the SAME handle stays a no-op, and the map
// still attaches with the handle in place — the release-on-overwrite path
// must not release a handle that is being kept.
func TestUnit_CrossStage_SameKeySameHandleOverwrite_Keeps(t *testing.T) {
	doc := newTestDoc(1)
	root := doc.GetMap("root")
	mp := NewMapPrelim()
	h := NewTextPrelim()
	doc.Transact(func(txn *Transaction) {
		mp.Set(txn, "k", h)
		mp.Set(txn, "k", h) // same key, same handle: keep, not release-then-lose
		h.Insert(txn, 0, "kept", nil)
		root.Set(txn, "mp", mp)
	})
	got, _ := mp.Get("k")
	txt, ok := got.(*YText)
	if !ok || txt.ToString() != "kept" {
		t.Fatalf("mp.k = %v, want the kept YText reading %q", got, "kept")
	}
}
