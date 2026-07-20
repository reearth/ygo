package mobile

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestInsertAndFormat_RoundTripConvergence exercises InsertText + FormatText on
// docA, syncs the full state to docB, and asserts both converge to the same
// idiomatic delta.
func TestInsertAndFormat_RoundTripConvergence(t *testing.T) {
	a := NewDoc()
	if err := a.InsertText("t", 0, "hello"); err != nil {
		t.Fatalf("InsertText: %v", err)
	}
	if err := a.FormatText("t", 0, 5, []byte(`{"bold":true}`)); err != nil {
		t.Fatalf("FormatText: %v", err)
	}

	aJSON, err := a.GetTextJSON("t")
	if err != nil {
		t.Fatalf("docA GetTextJSON: %v", err)
	}
	want := `[{"insert":"hello","attributes":{"bold":true}}]`
	if !jsonEqual(t, aJSON, []byte(want)) {
		t.Fatalf("docA GetTextJSON = %s, want %s", aJSON, want)
	}

	b := NewDoc()
	if err := b.ApplyUpdate(a.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("docB ApplyUpdate: %v", err)
	}
	bJSON, err := b.GetTextJSON("t")
	if err != nil {
		t.Fatalf("docB GetTextJSON: %v", err)
	}
	if !jsonEqual(t, aJSON, bJSON) {
		t.Fatalf("convergence mismatch:\n  docA: %s\n  docB: %s", aJSON, bJSON)
	}
}

// TestInsertTextWithAttributes asserts the attributed-insert method carries the
// caller's attributes through to the delta.
func TestInsertTextWithAttributes(t *testing.T) {
	m := NewDoc()
	if err := m.InsertTextWithAttributes("t", 0, "hi", []byte(`{"italic":true}`)); err != nil {
		t.Fatalf("InsertTextWithAttributes: %v", err)
	}
	got, err := m.GetTextJSON("t")
	if err != nil {
		t.Fatalf("GetTextJSON: %v", err)
	}
	want := `[{"insert":"hi","attributes":{"italic":true}}]`
	if !jsonEqual(t, got, []byte(want)) {
		t.Fatalf("GetTextJSON = %s, want %s", got, want)
	}
}

// TestFormatText_NullAttributeRemoval asserts that a null attribute value
// (Yjs's formatting-removal convention) actually clears the attribute on the
// formatted range.
func TestFormatText_NullAttributeRemoval(t *testing.T) {
	m := NewDoc()
	if err := m.InsertTextWithAttributes("t", 0, "hi", []byte(`{"bold":true}`)); err != nil {
		t.Fatalf("InsertTextWithAttributes: %v", err)
	}
	before, err := m.GetTextJSON("t")
	if err != nil {
		t.Fatalf("GetTextJSON (before): %v", err)
	}
	if !jsonEqual(t, before, []byte(`[{"insert":"hi","attributes":{"bold":true}}]`)) {
		t.Fatalf("GetTextJSON (before) = %s, want bold present", before)
	}

	// A null value removes the attribute (not a literal {"bold":null} in the map).
	if err := m.FormatText("t", 0, 2, []byte(`{"bold":null}`)); err != nil {
		t.Fatalf("FormatText (removal): %v", err)
	}
	after, err := m.GetTextJSON("t")
	if err != nil {
		t.Fatalf("GetTextJSON (after): %v", err)
	}
	if !jsonEqual(t, after, []byte(`[{"insert":"hi"}]`)) {
		t.Fatalf("GetTextJSON (after) = %s, want bold removed ([{\"insert\":\"hi\"}])", after)
	}

	// Defensive: no op in the resulting delta may still carry bold:true.
	var ops []map[string]any
	if err := json.Unmarshal(after, &ops); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	for _, op := range ops {
		if attrs, ok := op["attributes"].(map[string]any); ok {
			if attrs["bold"] == true {
				t.Fatalf("bold still present after removal: %s", after)
			}
		}
	}
}

// TestMutators_Validation asserts every bad-argument path returns a non-nil
// error (and does not panic — a panic would fail the test outright).
func TestMutators_Validation(t *testing.T) {
	cases := []struct {
		name string
		call func(m *Doc) error
	}{
		{"insert negative index", func(m *Doc) error { return m.InsertText("t", -1, "x") }},
		{"insert index past end", func(m *Doc) error { return m.InsertText("t", 5, "x") }},
		{"insert huge index", func(m *Doc) error { return m.InsertText("t", math.MaxInt32+1, "x") }},
		{"insertattrs negative index", func(m *Doc) error {
			return m.InsertTextWithAttributes("t", -1, "x", []byte(`{"b":true}`))
		}},
		{"insertattrs malformed json", func(m *Doc) error {
			return m.InsertTextWithAttributes("t", 0, "x", []byte("{"))
		}},
		{"delete negative index", func(m *Doc) error { return m.DeleteText("t", -1, 1) }},
		{"delete negative length", func(m *Doc) error { return m.DeleteText("t", 0, -1) }},
		{"delete length past end", func(m *Doc) error { return m.DeleteText("t", 0, 5) }},
		{"delete huge length", func(m *Doc) error { return m.DeleteText("t", 0, math.MaxInt32+1) }},
		{"format negative index", func(m *Doc) error { return m.FormatText("t", -1, 1, []byte(`{"b":true}`)) }},
		{"format range past end", func(m *Doc) error { return m.FormatText("t", 0, 5, []byte(`{"b":true}`)) }},
		{"format huge length", func(m *Doc) error {
			return m.FormatText("t", 0, math.MaxInt32+1, []byte(`{"b":true}`))
		}},
		{"format malformed json", func(m *Doc) error { return m.FormatText("t", 0, 0, []byte("{")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewDoc() // empty doc: Len("t") == 0
			if err := tc.call(m); err == nil {
				t.Fatalf("%s: expected non-nil error, got nil", tc.name)
			}
			// The invalid op must not have mutated the doc.
			got, err := m.GetTextJSON("t")
			if err != nil {
				t.Fatalf("GetTextJSON: %v", err)
			}
			if string(got) != "[]" {
				t.Fatalf("%s: doc mutated by invalid op: %s", tc.name, got)
			}
		})
	}
}

// TestMutators_AfterClose asserts every mutator returns ErrClosed after Close.
func TestMutators_AfterClose(t *testing.T) {
	m := NewDoc()
	m.Close()
	if err := m.InsertText("t", 0, "x"); err != ErrClosed {
		t.Errorf("InsertText after Close = %v, want ErrClosed", err)
	}
	if err := m.InsertTextWithAttributes("t", 0, "x", []byte(`{"b":true}`)); err != ErrClosed {
		t.Errorf("InsertTextWithAttributes after Close = %v, want ErrClosed", err)
	}
	if err := m.DeleteText("t", 0, 1); err != ErrClosed {
		t.Errorf("DeleteText after Close = %v, want ErrClosed", err)
	}
	if err := m.FormatText("t", 0, 1, []byte(`{"b":true}`)); err != ErrClosed {
		t.Errorf("FormatText after Close = %v, want ErrClosed", err)
	}
	if err := m.InsertArray("a", 0, []byte(`[1]`)); err != ErrClosed {
		t.Errorf("InsertArray after Close = %v, want ErrClosed", err)
	}
	if err := m.DeleteArray("a", 0, 1); err != ErrClosed {
		t.Errorf("DeleteArray after Close = %v, want ErrClosed", err)
	}
	if err := m.SetMap("m", "k", []byte(`1`)); err != ErrClosed {
		t.Errorf("SetMap after Close = %v, want ErrClosed", err)
	}
	if err := m.DeleteMapKey("m", "k"); err != ErrClosed {
		t.Errorf("DeleteMapKey after Close = %v, want ErrClosed", err)
	}
}

// TestInsertArray_RoundTripConvergence inserts a heterogeneous array on docA,
// syncs the full state to docB, and asserts docB's YArray JSON matches docA's
// (shape-normalized: JSON numbers decode to float64 on both sides).
func TestInsertArray_RoundTripConvergence(t *testing.T) {
	a := NewDoc()
	if err := a.InsertArray("a", 0, []byte(`[1,"x",true]`)); err != nil {
		t.Fatalf("InsertArray: %v", err)
	}
	aJSON, err := a.GetArrayJSON("a")
	if err != nil {
		t.Fatalf("docA GetArrayJSON: %v", err)
	}
	if !jsonEqual(t, aJSON, []byte(`[1,"x",true]`)) {
		t.Fatalf("docA GetArrayJSON = %s, want [1,\"x\",true]", aJSON)
	}

	b := NewDoc()
	if err := b.ApplyUpdate(a.EncodeStateAsUpdate()); err != nil {
		t.Fatalf("docB ApplyUpdate: %v", err)
	}
	bJSON, err := b.GetArrayJSON("a")
	if err != nil {
		t.Fatalf("docB GetArrayJSON: %v", err)
	}
	if !jsonEqual(t, aJSON, bJSON) {
		t.Fatalf("convergence mismatch:\n  docA: %s\n  docB: %s", aJSON, bJSON)
	}
}

// TestDeleteArray_SubRange deletes an interior sub-range and asserts the
// surviving elements are exactly those outside the range.
func TestDeleteArray_SubRange(t *testing.T) {
	m := NewDoc()
	if err := m.InsertArray("a", 0, []byte(`[0,1,2,3,4]`)); err != nil {
		t.Fatalf("InsertArray: %v", err)
	}
	if err := m.DeleteArray("a", 1, 2); err != nil { // remove elements 1,2
		t.Fatalf("DeleteArray: %v", err)
	}
	got, err := m.GetArrayJSON("a")
	if err != nil {
		t.Fatalf("GetArrayJSON: %v", err)
	}
	if !jsonEqual(t, got, []byte(`[0,3,4]`)) {
		t.Fatalf("GetArrayJSON = %s, want [0,3,4]", got)
	}
}

// TestSetMap_And_DeleteMapKey sets a key to a JSON object, asserts the map JSON
// carries it, then deletes the key and asserts the map is empty.
func TestSetMap_And_DeleteMapKey(t *testing.T) {
	m := NewDoc()
	if err := m.SetMap("m", "k", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("SetMap: %v", err)
	}
	got, err := m.GetMapJSON("m")
	if err != nil {
		t.Fatalf("GetMapJSON: %v", err)
	}
	if !jsonEqual(t, got, []byte(`{"k":{"n":1}}`)) {
		t.Fatalf("GetMapJSON = %s, want {\"k\":{\"n\":1}}", got)
	}

	if err := m.DeleteMapKey("m", "k"); err != nil {
		t.Fatalf("DeleteMapKey: %v", err)
	}
	got, err = m.GetMapJSON("m")
	if err != nil {
		t.Fatalf("GetMapJSON (after delete): %v", err)
	}
	if !jsonEqual(t, got, []byte(`{}`)) {
		t.Fatalf("GetMapJSON (after delete) = %s, want {}", got)
	}
}

// TestArrayMapMutators_Validation asserts every bad-argument path on the
// YArray/YMap mutators returns a non-nil error (no panic) and leaves the doc
// unmutated. Each case runs on an empty doc: array "a" stays [], map "m" stays {}.
func TestArrayMapMutators_Validation(t *testing.T) {
	cases := []struct {
		name string
		call func(m *Doc) error
	}{
		{"insertarray malformed json", func(m *Doc) error { return m.InsertArray("a", 0, []byte("[")) }},
		{"insertarray index past end", func(m *Doc) error { return m.InsertArray("a", 1, []byte(`[1]`)) }},
		{"insertarray negative index", func(m *Doc) error { return m.InsertArray("a", -1, []byte(`[1]`)) }},
		{"insertarray huge index", func(m *Doc) error { return m.InsertArray("a", math.MaxInt32+1, []byte(`[1]`)) }},
		{"deletearray range past end", func(m *Doc) error { return m.DeleteArray("a", 0, 5) }},
		{"deletearray negative index", func(m *Doc) error { return m.DeleteArray("a", -1, 1) }},
		{"deletearray negative length", func(m *Doc) error { return m.DeleteArray("a", 0, -1) }},
		{"setmap malformed json", func(m *Doc) error { return m.SetMap("m", "k", []byte("{")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewDoc() // empty doc: Len("a") == 0
			if err := tc.call(m); err == nil {
				t.Fatalf("%s: expected non-nil error, got nil", tc.name)
			}
			gotA, err := m.GetArrayJSON("a")
			if err != nil {
				t.Fatalf("GetArrayJSON: %v", err)
			}
			if !jsonEqual(t, gotA, []byte(`[]`)) {
				t.Fatalf("%s: array mutated by invalid op: %s", tc.name, gotA)
			}
			gotM, err := m.GetMapJSON("m")
			if err != nil {
				t.Fatalf("GetMapJSON: %v", err)
			}
			if !jsonEqual(t, gotM, []byte(`{}`)) {
				t.Fatalf("%s: map mutated by invalid op: %s", tc.name, gotM)
			}
		})
	}
}

// TestMutators_YjsInterop performs the same YText edits (insert "hi", bold the
// range) via the mobile mutators and in real Yjs, then asserts GetTextJSON
// equals yjs toDelta(). Gated by requireNodeYjs.
func TestMutators_YjsInterop(t *testing.T) {
	nodePath, testutilDir := requireNodeYjs(t)

	m := NewDoc()
	if err := m.InsertText("t", 0, "hi"); err != nil {
		t.Fatalf("InsertText: %v", err)
	}
	if err := m.FormatText("t", 0, 2, []byte(`{"bold":true}`)); err != nil {
		t.Fatalf("FormatText: %v", err)
	}
	goDelta, err := m.GetTextJSON("t")
	if err != nil {
		t.Fatalf("GetTextJSON: %v", err)
	}

	// A script file resolves `require` relative to its own directory, so pass the
	// absolute yjs path as argv[2] (same trick as the json_test interop test).
	yjsPath, err := filepath.Abs(filepath.Join(testutilDir, "node_modules", "yjs"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	// yjs performs the same logical edits and prints its idiomatic delta.
	script := `
const Y = require(process.argv[2]);
const doc = new Y.Doc();
const t = doc.getText('t');
t.insert(0, 'hi');
t.format(0, 2, { bold: true });
process.stdout.write(JSON.stringify(t.toDelta()));
`
	scriptPath := filepath.Join(dir, "delta.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(nodePath, scriptPath, yjsPath)
	cmd.Dir = testutilDir
	yjsDelta, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("node yjs toDelta failed: %v\n%s", err, stderr)
	}

	if !jsonEqual(t, goDelta, yjsDelta) {
		t.Fatalf("delta mismatch:\n  go:  %s\n  yjs: %s", goDelta, yjsDelta)
	}
}
