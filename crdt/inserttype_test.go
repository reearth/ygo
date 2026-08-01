package crdt

import (
	"encoding/json"
	"testing"
)

// pushCells fills an array with plain-sourced cells so the InsertType tests
// have physical neighbours to anchor against.
func pushCells(doc *Doc, arr *YArray, texts ...string) {
	doc.Transact(func(txn *Transaction) {
		for _, text := range texts {
			cell := NewMapPrelim()
			body := NewTextPrelim()
			body.Insert(txn, 0, text, nil)
			cell.Set(txn, "source", body)
			arr.PushType(txn, cell)
		}
	})
}

func sourcesOf(t *testing.T, arr *YArray) []string {
	t.Helper()
	raw, err := arr.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	out := make([]string, 0, len(decoded))
	for _, c := range decoded {
		s, _ := c["source"].(string)
		out = append(out, s)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestInsertTypePlacesANestedTypeAtAnIndex(t *testing.T) {
	// The case Move used to serve: a nested type in the MIDDLE of an array.
	// Move is not an option — it emits ContentMove, a ygo extension no other
	// Yjs implementation decodes (#207) — so the placement has to happen at
	// insert.
	src := New()
	cells := src.GetArray("cells")
	pushCells(src, cells, "a", "b", "c")

	src.Transact(func(txn *Transaction) {
		note := NewMapPrelim()
		body := NewTextPrelim()
		body.Insert(txn, 0, "note", nil)
		note.Set(txn, "source", body)
		cells.InsertType(txn, 1, note)
	})

	want := []string{"a", "note", "b", "c"}
	if got := sourcesOf(t, cells); !equalStrings(got, want) {
		t.Fatalf("local order = %v, want %v", got, want)
	}

	// And it must survive the wire — the update is where a bad anchor shows up.
	dst := New()
	if err := dst.ApplyUpdate(src.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if got := sourcesOf(t, dst.GetArray("cells")); !equalStrings(got, want) {
		t.Fatalf("decoded order = %v, want %v", got, want)
	}
}

func TestInsertTypeAtTheEndsAndIntoAnEmptyArray(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start []string
		index int
		want  []string
	}{
		{"prepend", []string{"a", "b"}, 0, []string{"note", "a", "b"}},
		{"append", []string{"a", "b"}, 2, []string{"a", "b", "note"}},
		{"beyond the end", []string{"a", "b"}, 10, []string{"a", "b", "note"}},
		{"negative", []string{"a", "b"}, -1, []string{"a", "b", "note"}},
		{"empty", nil, 0, []string{"note"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := New()
			arr := doc.GetArray("cells")
			if len(tc.start) > 0 {
				pushCells(doc, arr, tc.start...)
			}
			doc.Transact(func(txn *Transaction) {
				note := NewMapPrelim()
				body := NewTextPrelim()
				body.Insert(txn, 0, "note", nil)
				note.Set(txn, "source", body)
				arr.InsertType(txn, tc.index, note)
			})
			if got := sourcesOf(t, arr); !equalStrings(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
			dst := New()
			if err := dst.ApplyUpdate(doc.EncodeStateAsUpdate()); err != nil {
				t.Fatalf("ApplyUpdate: %v", err)
			}
			if got := sourcesOf(t, dst.GetArray("cells")); !equalStrings(got, tc.want) {
				t.Fatalf("decoded order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInsertTypeSplitsAPlainValueRun(t *testing.T) {
	// Plain values batch into ONE ContentAny item, so inserting between two of
	// them has to split that item — the offset > 0 branch, which the
	// all-nested-types cases never reach.
	doc := New()
	arr := doc.GetArray("mixed")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x", "y", "z"}) })
	doc.Transact(func(txn *Transaction) {
		m := NewMapPrelim()
		m.Set(txn, "kind", "note")
		arr.InsertType(txn, 2, m)
	})

	dst := New()
	if err := dst.ApplyUpdate(doc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	got, err := dst.GetArray("mixed").ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded []any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decoding %s: %v", got, err)
	}
	if len(decoded) != 4 || decoded[0] != "x" || decoded[1] != "y" || decoded[3] != "z" {
		t.Fatalf("decoded = %v, want x y {note} z", decoded)
	}
	m, ok := decoded[2].(map[string]any)
	if !ok || m["kind"] != "note" {
		t.Fatalf("element 2 = %#v, want the inserted map", decoded[2])
	}
}

func TestInsertTypeRejectsAnAttachedType(t *testing.T) {
	doc := New()
	a, b := doc.GetArray("a"), doc.GetArray("b")
	m := NewMapPrelim()
	doc.Transact(func(txn *Transaction) { a.PushType(txn, m) })

	defer func() {
		if recover() == nil {
			t.Fatal("InsertType accepted an already-attached type; want panic")
		}
	}()
	doc.Transact(func(txn *Transaction) { b.InsertType(txn, 0, m) })
}

func TestInsertTypeStagesWhenArrayDetached(t *testing.T) {
	// On a detached array InsertType splices the type into the staged content,
	// readable immediately and materialised at attach in staged order — with
	// plain-value runs splitting around it at flush.
	doc := New()
	root := doc.GetMap("root")
	outer := NewArrayPrelim()

	doc.Transact(func(txn *Transaction) {
		outer.Push(txn, []any{"x", "y"})
		head := NewMapPrelim()
		head.Set(txn, "kind", "head")
		outer.InsertType(txn, 1, head) // staged: outer is still detached
	})
	if got := outer.Len(); got != 3 {
		t.Fatalf("detached array has len %d, want 3 (staged InsertType must be readable)", got)
	}
	mid, _ := outer.Get(1).(*YMap)
	if mid == nil {
		t.Fatalf("detached Get(1) = %v, want the staged *YMap handle", outer.Get(1))
	}
	if got := root.Keys(); len(got) != 0 {
		t.Fatalf("root has keys %v before attach, want none (staging must not materialise)", got)
	}

	doc.Transact(func(txn *Transaction) { root.Set(txn, "cells", outer) })
	if got := outer.Len(); got != 3 {
		t.Fatalf("attached array has len %d, want 3", got)
	}
	attached, _ := outer.Get(1).(*YMap)
	if attached == nil {
		t.Fatal("item 1 is not a map after attach")
	}
	if v, _ := attached.Get("kind"); v != "head" {
		t.Fatalf("item 1 kind = %v, want head (staged InsertType must keep its position)", v)
	}

	// And the wire agrees.
	dst := New()
	if err := dst.ApplyUpdate(doc.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	raw, err := dst.GetMap("root").ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	cells, _ := m["cells"].([]any)
	if len(cells) != 3 || cells[0] != "x" || cells[2] != "y" {
		t.Fatalf("decoded cells = %v, want [x {kind:head} y]", cells)
	}
	if mm, ok := cells[1].(map[string]any); !ok || mm["kind"] != "head" {
		t.Fatalf("decoded cells[1] = %#v, want the staged map", cells[1])
	}
}

func TestInsertTypeBoundariesMatchAttachedWhenStaged(t *testing.T) {
	// Same discipline as TestDetachedBoundariesMatchAttached: an InsertType
	// staged on a detached array must land exactly where an attached
	// InsertType lands, for every boundary shape.
	for _, tc := range []struct {
		name  string
		index int
	}{
		{"interior", 1},
		{"prepend", 0},
		{"append", 3},
		{"beyond the end", 42},
		{"negative", -3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attachedDoc := New()
			attached := attachedDoc.GetArray("a")
			attachedDoc.Transact(func(txn *Transaction) {
				attached.Push(txn, []any{"a", "b", "c"})
				m := NewMapPrelim()
				m.Set(txn, "k", "v")
				attached.InsertType(txn, tc.index, m)
			})

			stagedDoc := New()
			root := stagedDoc.GetArray("root")
			staged := NewArrayPrelim()
			stagedDoc.Transact(func(txn *Transaction) {
				staged.Push(txn, []any{"a", "b", "c"})
				m := NewMapPrelim()
				m.Set(txn, "k", "v")
				staged.InsertType(txn, tc.index, m)
				root.PushType(txn, staged)
			})

			want, err := attached.ToJSON()
			if err != nil {
				t.Fatal(err)
			}
			got, err := staged.ToJSON()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("attached %s, staged %s — boundary semantics must agree", want, got)
			}
		})
	}
}
