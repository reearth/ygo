package mobile

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
)

// TestDeltaToIdiomaticJSON covers the op-kind translation from ygo's internal
// crdt.Delta shape to the idiomatic Yjs delta shape.
func TestDeltaToIdiomaticJSON(t *testing.T) {
	cases := []struct {
		name  string
		delta []crdt.Delta
		want  string
	}{
		{
			name:  "insert with attributes",
			delta: []crdt.Delta{{Op: crdt.DeltaOpInsert, Insert: "hi", Attributes: crdt.Attributes{"bold": true}}},
			want:  `[{"insert":"hi","attributes":{"bold":true}}]`,
		},
		{
			name:  "insert without attributes drops the key",
			delta: []crdt.Delta{{Op: crdt.DeltaOpInsert, Insert: "hi"}},
			want:  `[{"insert":"hi"}]`,
		},
		{
			name:  "insert with empty (non-nil) attributes drops the key",
			delta: []crdt.Delta{{Op: crdt.DeltaOpInsert, Insert: "hi", Attributes: crdt.Attributes{}}},
			want:  `[{"insert":"hi"}]`,
		},
		{
			name:  "retain",
			delta: []crdt.Delta{{Op: crdt.DeltaOpRetain, Retain: 3}},
			want:  `[{"retain":3}]`,
		},
		{
			name:  "delete",
			delta: []crdt.Delta{{Op: crdt.DeltaOpDelete, Delete: 2}},
			want:  `[{"delete":2}]`,
		},
		{
			name:  "embed: non-string insert value passes through as an object",
			delta: []crdt.Delta{{Op: crdt.DeltaOpInsert, Insert: map[string]any{"image": "http://x/y.png"}}},
			want:  `[{"insert":{"image":"http://x/y.png"}}]`,
		},
		{
			name:  "nil slice becomes [] not null",
			delta: nil,
			want:  `[]`,
		},
		{
			name:  "empty slice becomes []",
			delta: []crdt.Delta{},
			want:  `[]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deltaToIdiomaticJSON(tc.delta)
			if err != nil {
				t.Fatalf("deltaToIdiomaticJSON: %v", err)
			}
			if !jsonEqual(t, got, []byte(tc.want)) {
				t.Fatalf("deltaToIdiomaticJSON = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestGetTextJSON_Idiomatic asserts the wrapper emits the idiomatic Yjs delta
// shape rather than ygo's capitalized crdt.Delta struct shape.
func TestGetTextJSON_Idiomatic(t *testing.T) {
	m := NewDoc()
	m.d.Transact(func(txn *crdt.Transaction) {
		txn.GetText("t").Insert(txn, 0, "hi", crdt.Attributes{"bold": true})
	})

	got, err := m.GetTextJSON("t")
	if err != nil {
		t.Fatalf("GetTextJSON: %v", err)
	}
	want := `[{"insert":"hi","attributes":{"bold":true}}]`
	if !jsonEqual(t, got, []byte(want)) {
		t.Fatalf("GetTextJSON = %s, want %s", got, want)
	}

	// Absent root -> [] (not null), so a JS consumer can .forEach safely.
	empty, err := m.GetTextJSON("nope")
	if err != nil {
		t.Fatalf("GetTextJSON(absent): %v", err)
	}
	if string(empty) != "[]" {
		t.Fatalf("GetTextJSON(absent) = %s, want []", empty)
	}
}

// TestStatesJSON_Idiomatic asserts the awareness state map is keyed by stringy
// client ID with the raw state object as the value — no Clock/clock wrapper.
func TestStatesJSON_Idiomatic(t *testing.T) {
	// Empty awareness -> {} (not null).
	w, err := NewAwareness(1)
	if err != nil {
		t.Fatalf("NewAwareness: %v", err)
	}
	empty, err := w.StatesJSON()
	if err != nil {
		t.Fatalf("StatesJSON (empty): %v", err)
	}
	if string(empty) != "{}" {
		t.Fatalf("StatesJSON (empty) = %s, want {}", empty)
	}

	// Client 1 sets state directly; client 2's state is merged in via an update.
	w.a.SetLocalState(map[string]any{"user": "alice"})
	other := awareness.New(2)
	other.SetLocalState(map[string]any{"user": "bob"})
	if err := w.a.ApplyUpdate(other.EncodeUpdate([]uint64{2}), nil); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	got, err := w.StatesJSON()
	if err != nil {
		t.Fatalf("StatesJSON: %v", err)
	}
	want := `{"1":{"user":"alice"},"2":{"user":"bob"}}`
	if !jsonEqual(t, got, []byte(want)) {
		t.Fatalf("StatesJSON = %s, want %s (must not include Clock)", got, want)
	}
}

// TestGetTextJSON_YjsInterop builds a rich-text document (a formatting attribute
// plus an embed), encodes it, and asserts real Yjs decodes it and renders the
// SAME idiomatic delta that GetTextJSON emits. Gated by requireNodeYjs.
func TestGetTextJSON_YjsInterop(t *testing.T) {
	nodePath, testutilDir := requireNodeYjs(t)

	m := NewDoc()
	m.d.Transact(func(txn *crdt.Transaction) {
		txt := txn.GetText("t")
		txt.Insert(txn, 0, "hi", crdt.Attributes{"bold": true})
		txt.InsertEmbed(txn, 2, map[string]any{"image": "http://x/y.png"}, nil)
	})

	update := m.EncodeStateAsUpdate()
	if update == nil {
		t.Fatal("EncodeStateAsUpdate returned nil")
	}

	// A script file resolves `require` relative to its own directory, not the
	// cwd, so pass the absolute yjs path as an argument (the crdt compat tests
	// use the same trick). argv: [node, script, yjsPath, updatePath].
	yjsPath, err := filepath.Abs(filepath.Join(testutilDir, "node_modules", "yjs"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	updatePath := filepath.Join(dir, "update.bin")
	if err := os.WriteFile(updatePath, update, 0o644); err != nil {
		t.Fatal(err)
	}

	// Node reads the update bytes, applies them to a fresh Y.Doc (clientID is
	// irrelevant for decoding), and prints the idiomatic delta as JSON.
	script := `
const Y = require(process.argv[2]);
const fs = require('fs');
const doc = new Y.Doc();
Y.applyUpdate(doc, new Uint8Array(fs.readFileSync(process.argv[3])));
process.stdout.write(JSON.stringify(doc.getText('t').toDelta()));
`
	scriptPath := filepath.Join(dir, "delta.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(nodePath, scriptPath, yjsPath, updatePath)
	cmd.Dir = testutilDir
	yjsDelta, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("node yjs toDelta failed: %v\n%s", err, stderr)
	}

	goDelta, err := m.GetTextJSON("t")
	if err != nil {
		t.Fatalf("GetTextJSON: %v", err)
	}
	if !jsonEqual(t, goDelta, yjsDelta) {
		t.Fatalf("delta mismatch:\n  go:  %s\n  yjs: %s", goDelta, yjsDelta)
	}
}
