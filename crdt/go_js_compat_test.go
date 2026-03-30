package crdt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestCompat_GoToJS encodes several documents with Go and verifies that the
// Yjs reference implementation (Node.js) can decode and read them correctly.
//
// Requires node to be available on PATH. The test is skipped when node is absent
// so it does not break headless CI environments without Node.js.
func TestCompat_GoToJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found on PATH — skipping Go→JS interop test")
	}

	fixtureDir := filepath.Join("..", "testutil", "go_fixtures")
	require.NoError(t, os.MkdirAll(fixtureDir, 0755))

	write := func(name string, data []byte) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(fixtureDir, name+".bin"), data, 0644))
	}

	// ── YText: simple insert ─────────────────────────────────────────────────
	{
		doc := crdt.New(crdt.WithClientID(1))
		txt := doc.GetText("content")
		doc.Transact(func(txn *crdt.Transaction) {
			txt.Insert(txn, 0, "Hello from Go!", nil)
		})
		write("ytext_insert_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
		write("ytext_insert_v2", crdt.EncodeStateAsUpdateV2(doc, nil))
	}

	// ── YText: insert + delete ───────────────────────────────────────────────
	{
		doc := crdt.New(crdt.WithClientID(1))
		txt := doc.GetText("content")
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "Hello, world!", nil) })
		doc.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 5, 7) }) // → "Hello!"
		write("ytext_delete_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
	}

	// ── YText: bold formatting ───────────────────────────────────────────────
	{
		doc := crdt.New(crdt.WithClientID(1))
		txt := doc.GetText("content")
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "Hello, world!", nil) })
		doc.Transact(func(txn *crdt.Transaction) {
			txt.Format(txn, 0, 5, crdt.Attributes{"bold": true})
		})
		write("ytext_format_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
	}

	// ── YArray: mixed types ──────────────────────────────────────────────────
	{
		doc := crdt.New(crdt.WithClientID(1))
		arr := doc.GetArray("list")
		doc.Transact(func(txn *crdt.Transaction) {
			arr.Insert(txn, 0, []any{1, "two", true, nil, map[string]any{"key": "val"}})
		})
		write("yarray_mixed_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
	}

	// ── YMap: basic ──────────────────────────────────────────────────────────
	{
		doc := crdt.New(crdt.WithClientID(1))
		m := doc.GetMap("data")
		doc.Transact(func(txn *crdt.Transaction) {
			m.Set(txn, "name", "Alice")
			m.Set(txn, "age", 30)
			m.Set(txn, "active", true)
		})
		write("ymap_basic_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
	}

	// ── Concurrent merge (two Go clients) ────────────────────────────────────
	{
		docA := crdt.New(crdt.WithClientID(10))
		docB := crdt.New(crdt.WithClientID(20))
		txtA := docA.GetText("t")
		txtB := docB.GetText("t")
		docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "Alice", nil) })
		docB.Transact(func(txn *crdt.Transaction) { txtB.Insert(txn, 0, "Bob", nil) })
		uA := crdt.EncodeStateAsUpdateV1(docA, nil)
		uB := crdt.EncodeStateAsUpdateV1(docB, nil)
		merged, err := crdt.MergeUpdatesV1(uA, uB)
		require.NoError(t, err)
		write("concurrent_merge_v1", merged)
	}

	// ── Run-length squash: 5 individual inserts → 1 item ─────────────────────
	{
		doc := crdt.New(crdt.WithClientID(1))
		txt := doc.GetText("content")
		// Insert each character in the same transaction → squashed into 1 item.
		doc.Transact(func(txn *crdt.Transaction) {
			txt.Insert(txn, 0, "h", nil)
			txt.Insert(txn, 1, "e", nil)
			txt.Insert(txn, 2, "l", nil)
			txt.Insert(txn, 3, "l", nil)
			txt.Insert(txn, 4, "o", nil)
		})
		write("squashed_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
	}

	// ── Run Node.js verifier ─────────────────────────────────────────────────
	script := filepath.Join("..", "testutil", "verify_go_fixtures.js")
	cmd := exec.Command(nodePath, script)
	cmd.Dir = filepath.Join("..", "testutil")
	out, err := cmd.CombinedOutput()
	t.Logf("node output:\n%s", out)
	require.NoError(t, err, "Go→JS interop verification failed")
}
