package crdt_test

import (
	"encoding/hex"
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

	// ── YMap: last-write-wins (key overwritten) ──────────────────────────────
	// Encode-side conformance for the duplicate-key wire bug: the second Set
	// produces an origin-bearing item whose parentSub must NOT be written to
	// the wire. If ygo regresses to writing it, real Yjs decode misaligns.
	{
		doc := crdt.New(crdt.WithClientID(1))
		m := doc.GetMap("m")
		doc.Transact(func(txn *crdt.Transaction) { m.Set(txn, "k", 1) })
		doc.Transact(func(txn *crdt.Transaction) { m.Set(txn, "k", 2) })
		write("ymap_lww_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
		write("ymap_lww_v2", crdt.EncodeStateAsUpdateV2(doc, nil))
	}

	// ── YMap: empty-string key ────────────────────────────────────────────────
	{
		doc := crdt.New(crdt.WithClientID(1))
		m := doc.GetMap("m")
		doc.Transact(func(txn *crdt.Transaction) { m.Set(txn, "", "value") })
		write("ymap_empty_key_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
		write("ymap_empty_key_v2", crdt.EncodeStateAsUpdateV2(doc, nil))
	}

	// ── YText embed: encode-side wire conformance ────────────────────────────
	// Yjs V1 decodes the embed value as a JSON-text varstring; if ygo regresses
	// to WriteAny, real Yjs fails. Verifies both V1 and V2 embed output.
	{
		doc := crdt.New(crdt.WithClientID(1))
		txt := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) {
			txt.Insert(txn, 0, "ab", nil)
			txt.InsertEmbed(txn, 1, map[string]any{"image": "http://x/y.png", "w": 3}, nil)
		})
		write("ytext_embed_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
		write("ytext_embed_v2", crdt.EncodeStateAsUpdateV2(doc, nil))
	}

	// ── YText mid-surrogate split: encode-side wire conformance ──────────────
	// Inserting at an index that bisects a surrogate pair must split the emoji
	// into U+FFFD halves exactly like Yjs, so real Yjs reads back "a�X�c"
	// (verified against yjs@13.6.30). Guards splitUTF16 on the encode path.
	{
		doc := crdt.New(crdt.WithClientID(1))
		txt := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "a😀c", nil) })
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 2, "X", nil) }) // index 2 = mid-😀
		write("ytext_midsurrogate_v1", crdt.EncodeStateAsUpdateV1(doc, nil))
		write("ytext_midsurrogate_v2", crdt.EncodeStateAsUpdateV2(doc, nil))
	}

	// ── Subdoc re-encode: decode genuine Yjs subdoc bytes, re-encode with ygo,
	// and confirm real Yjs can still decode the result. Guards the ContentDoc
	// opts fix (Yjs crashes on a null/absent opts). ──────────────────────────
	{
		// Genuine yjs@13.6.30: m.set('child', new Y.Doc()) — V2 bytes.
		raw, herr := hex.DecodeString("0000059fc9ce8f02000001292e2a6d6368696c6435356566333938332d613238352d343139302d623234312d64643564306233663034386101052401010000010100760000")
		require.NoError(t, herr)
		d := crdt.New(crdt.WithClientID(1))
		require.NoError(t, crdt.ApplyUpdateV2(d, raw, nil))
		write("subdoc_reencode_v1", crdt.EncodeStateAsUpdateV1(d, nil))
		write("subdoc_reencode_v2", crdt.EncodeStateAsUpdateV2(d, nil))
	}

	// ── Mirror loop (yjs → ygo → yjs): ygo decodes genuine Yjs bytes, then
	// re-encodes; real Yjs must read the result back with the right content.
	// Proves ygo's encode of YJS-ORIGINATED structs (a different path than
	// encoding a Go-built doc) stays conformant. ────────────────────────────
	reencode := func(label, yjsV1Hex string) {
		raw, herr := hex.DecodeString(yjsV1Hex)
		require.NoError(t, herr)
		d := crdt.New(crdt.WithClientID(1))
		require.NoError(t, crdt.ApplyUpdateV1(d, raw, nil))
		write(label+"_v1", crdt.EncodeStateAsUpdateV1(d, nil))
		write(label+"_v2", crdt.EncodeStateAsUpdateV2(d, nil))
	}
	// dup-key {k:2} and an embed — genuine yjs@13.6.30 V1 bytes.
	reencode("ymap_dupkey_reencode", "0102a0dcabf704002101016d016b01a8a0dcabf70400017d0201a0dcabf704010001")
	reencode("embed_reencode", "0103bbab84b70d0004010174016184bbab84b70d000162c5bbab84b70d00bbab84b70d01207b22696d616765223a22687474703a2f2f782f792e706e67222c2277223a337d00")

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
