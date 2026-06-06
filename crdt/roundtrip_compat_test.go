package crdt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// TestCompat_RoundTrip_GoJSGo exercises the full two-hop loop:
//
//	ygo encodes → Yjs applies AND re-encodes → ygo decodes the re-encoding.
//
// The document content must be identical before and after the loop. This is
// stronger than the one-hop Go→JS / JS→Go checks: it catches anything Yjs
// normalises, reorders, or re-splits when it re-serialises (which the one-hop
// "does Yjs accept our bytes" check can't see). Runs for both V1 and V2.
//
// Requires node; skipped when absent so headless CI stays green.
func TestCompat_RoundTrip_GoJSGo(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found on PATH — skipping ygo→JS→ygo round-trip")
	}

	// content extracts the canonical JSON of the scenario's root, via ygo's
	// own ToJSON — so we compare ygo-before vs ygo-after through the same lens.
	type scenario struct {
		name    string
		build   func(*crdt.Doc)
		content func(*testing.T, *crdt.Doc) string
	}

	textJSON := func(t *testing.T, d *crdt.Doc) string {
		b, err := d.GetText("t").ToJSON()
		require.NoError(t, err)
		return string(b)
	}
	arrJSON := func(t *testing.T, d *crdt.Doc) string {
		b, err := d.GetArray("a").ToJSON()
		require.NoError(t, err)
		return string(b)
	}
	mapJSON := func(t *testing.T, d *crdt.Doc) string {
		b, err := d.GetMap("m").ToJSON()
		require.NoError(t, err)
		return string(b)
	}

	scenarios := []scenario{
		{"text_plain", func(d *crdt.Doc) {
			txt := d.GetText("t")
			d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "Hello, 世界 — ✨", nil) })
		}, textJSON},
		{"text_format", func(d *crdt.Doc) {
			txt := d.GetText("t")
			d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "Hello, world!", nil) })
			d.Transact(func(tx *crdt.Transaction) { txt.Format(tx, 0, 5, crdt.Attributes{"bold": true}) })
		}, textJSON},
		{"text_embed", func(d *crdt.Doc) {
			txt := d.GetText("t")
			d.Transact(func(tx *crdt.Transaction) {
				txt.Insert(tx, 0, "ab", nil)
				txt.InsertEmbed(tx, 1, map[string]any{"image": "http://x/y.png", "w": 3}, nil)
			})
		}, textJSON},
		{"text_delete", func(d *crdt.Doc) {
			txt := d.GetText("t")
			d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "Hello, world!", nil) })
			d.Transact(func(tx *crdt.Transaction) { txt.Delete(tx, 5, 7) })
		}, textJSON},
		{"array_mixed", func(d *crdt.Doc) {
			arr := d.GetArray("a")
			d.Transact(func(tx *crdt.Transaction) {
				arr.Push(tx, []any{1, "two", true, nil, map[string]any{"k": "v"}})
			})
		}, arrJSON},
		{"map_scalars", func(d *crdt.Doc) {
			m := d.GetMap("m")
			d.Transact(func(tx *crdt.Transaction) {
				m.Set(tx, "name", "Alice")
				m.Set(tx, "age", 30)
				m.Set(tx, "active", true)
			})
		}, mapJSON},
		{"map_dupkey", func(d *crdt.Doc) {
			m := d.GetMap("m")
			d.Transact(func(tx *crdt.Transaction) { m.Set(tx, "k", 1) })
			d.Transact(func(tx *crdt.Transaction) { m.Set(tx, "k", 2) })
		}, mapJSON},
		{"map_emptykey", func(d *crdt.Doc) {
			m := d.GetMap("m")
			d.Transact(func(tx *crdt.Transaction) {
				m.Set(tx, "", "E")
				m.Set(tx, "normal", "N")
			})
		}, mapJSON},
		{"map_delete", func(d *crdt.Doc) {
			m := d.GetMap("m")
			d.Transact(func(tx *crdt.Transaction) { m.Set(tx, "keep", 1); m.Set(tx, "gone", 2) })
			d.Transact(func(tx *crdt.Transaction) { m.Delete(tx, "gone") })
		}, mapJSON},
	}

	dir := t.TempDir()
	write := func(name string, b []byte) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o644))
	}

	// Hop 1: ygo encodes. Record the expected content from the source doc.
	want := make(map[string]string, len(scenarios))
	for _, sc := range scenarios {
		src := crdt.New(crdt.WithClientID(1))
		sc.build(src)
		want[sc.name] = sc.content(t, src)
		write(sc.name+".v1.in", crdt.EncodeStateAsUpdateV1(src, nil))
		write(sc.name+".v2.in", crdt.EncodeStateAsUpdateV2(src, nil))
	}

	// Hop 2: Yjs applies + re-encodes (node), writing the .out files.
	script := filepath.Join("..", "testutil", "reencode_roundtrip.js")
	cmd := exec.Command(nodePath, script, dir)
	cmd.Dir = filepath.Join("..", "testutil")
	out, err := cmd.CombinedOutput()
	t.Logf("node: %s", out)
	require.NoError(t, err, "Yjs re-encode hop failed")

	// Hop 3: ygo decodes Yjs's re-encoding and must reproduce the content.
	for _, sc := range scenarios {
		for _, ver := range []struct {
			tag   string
			apply func(*crdt.Doc, []byte, any) error
		}{
			{"v1", crdt.ApplyUpdateV1},
			{"v2", crdt.ApplyUpdateV2},
		} {
			reencoded, rerr := os.ReadFile(filepath.Join(dir, sc.name+"."+ver.tag+".out"))
			require.NoError(t, rerr, "%s/%s: Yjs produced no re-encoding", sc.name, ver.tag)

			dst := crdt.New(crdt.WithClientID(2))
			require.NoError(t, ver.apply(dst, reencoded, nil),
				"%s/%s: ygo failed to decode Yjs's re-encoding", sc.name, ver.tag)

			require.Equal(t, want[sc.name], sc.content(t, dst),
				"%s/%s: content changed across the ygo→yjs→ygo round-trip", sc.name, ver.tag)
		}
	}
}
