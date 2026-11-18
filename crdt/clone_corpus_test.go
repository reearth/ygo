package crdt

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── The corpus ───────────────────────────────────────────────────────────────

// cloneCase is one document of the Clone corpus: the document, the options it
// was built with (which a clone and an encode-and-apply copy both take), and a
// name.
type cloneCase struct {
	name string
	doc  *Doc
	opts []DocOption
}

// cloneCorpus is every document TestClone_Equivalence clones: each update the
// repo's fixture corpora hold, applied to a fresh document, and the generated
// documents below.
func cloneCorpus(t *testing.T) []cloneCase {
	t.Helper()
	cases := append(fixtureCloneCases(t), generatedCloneCases(t)...)
	require.NotEmpty(t, cases)
	return cases
}

// fixtureCloneCases applies every update the repo's fixture corpora hold: the
// JSON conformance fixtures in crdt/testdata, the binary fixtures in
// testutil/fixtures and testutil/go_fixtures, and the fuzz corpora in
// crdt/testdata/fuzz. A seed that does not apply cleanly is kept: the document
// it leaves - with the structs the apply parked, or those it integrated before
// failing - is a document a clone must copy too.
func fixtureCloneCases(t *testing.T) []cloneCase {
	t.Helper()
	var cases []cloneCase
	add := func(name string, update []byte, v2 bool) {
		doc := New(WithClientID(7))
		if v2 {
			_ = ApplyUpdateV2(doc, update, nil)
		} else {
			_ = ApplyUpdateV1(doc, update, nil)
		}
		cases = append(cases, cloneCase{name: name, doc: doc, opts: []DocOption{WithClientID(7)}})
	}

	jsonFiles, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, jsonFiles)
	for _, file := range jsonFiles {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		var tree any
		require.NoError(t, json.Unmarshal(raw, &tree), file)
		base := strings.TrimSuffix(filepath.Base(file), ".json")
		for _, u := range collectHexUpdates(tree, base) {
			add(u.name, u.update, false)
		}
	}

	for _, dir := range []string{
		filepath.Join("..", "testutil", "fixtures"),
		filepath.Join("..", "testutil", "go_fixtures"),
	} {
		bins, err := filepath.Glob(filepath.Join(dir, "*.bin"))
		require.NoError(t, err)
		for _, file := range bins {
			name := strings.TrimSuffix(filepath.Base(file), ".bin")
			raw, err := os.ReadFile(file)
			require.NoError(t, err)
			add(filepath.Base(dir)+"/"+name, raw, strings.HasSuffix(name, "_v2"))
		}
	}

	for _, target := range []struct {
		dir string
		v2  bool
	}{
		{filepath.Join("testdata", "fuzz", "FuzzApplyUpdateV1"), false},
		{filepath.Join("testdata", "fuzz", "FuzzApplyUpdateV2"), true},
	} {
		seeds, err := filepath.Glob(filepath.Join(target.dir, "*"))
		require.NoError(t, err)
		for _, file := range seeds {
			raw, err := os.ReadFile(file)
			require.NoError(t, err)
			for i, update := range goFuzzSeedBytes(string(raw)) {
				add(fmt.Sprintf("fuzz/%s/%s#%d", filepath.Base(target.dir), filepath.Base(file), i), update, target.v2)
			}
		}
	}
	return cases
}

// namedUpdate is one update found in a fixture file.
type namedUpdate struct {
	name   string
	update []byte
}

// collectHexUpdates walks a decoded fixture file and returns every hex-encoded
// update under a "v1" or "updateV1" key, whatever the file's shape: the
// corpora are lists of fixtures, maps of named lists, and lists holding
// per-peer encodings, and a new corpus is picked up without a change here.
func collectHexUpdates(node any, path string) []namedUpdate {
	var out []namedUpdate
	switch v := node.(type) {
	case map[string]any:
		name := path
		if s, ok := v["name"].(string); ok && s != "" {
			name = path + "/" + s
		}
		for _, key := range []string{"v1", "updateV1"} {
			s, ok := v[key].(string)
			if !ok || s == "" {
				continue
			}
			update, err := hex.DecodeString(s)
			if err != nil {
				continue
			}
			out = append(out, namedUpdate{name: name + "/" + key, update: update})
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "v1" || k == "updateV1" {
				continue
			}
			out = append(out, collectHexUpdates(v[k], name+"/"+k)...)
		}
	case []any:
		for i, child := range v {
			out = append(out, collectHexUpdates(child, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return out
}

// goFuzzSeedBytes returns the []byte values of a go test fuzz corpus file.
func goFuzzSeedBytes(file string) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(file, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[]byte(") || !strings.HasSuffix(line, ")") {
			continue
		}
		s, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(line, "[]byte("), ")"))
		if err != nil {
			continue
		}
		out = append(out, []byte(s))
	}
	return out
}

// generatedCloneCases builds the documents the fixtures do not cover: every
// shared type, formatting and embeds, replaced map keys, deletes with gc on
// and off, a merge of two clients' concurrent edits, a subdocument, moves, a
// document holding parked structs and parked deletes, and seeded random
// documents built from all of the above.
func generatedCloneCases(t *testing.T) []cloneCase {
	t.Helper()
	var cases []cloneCase
	add := func(name string, opts []DocOption, build func(*Doc)) {
		doc := New(opts...)
		build(doc)
		cases = append(cases, cloneCase{name: "gen/" + name, doc: doc, opts: opts})
	}
	one := []DocOption{WithClientID(1)}
	noGC := []DocOption{WithClientID(1), WithGC(false)}

	add("empty", one, func(d *Doc) {})

	add("text_one_transaction", one, func(d *Doc) {
		txt := d.GetText("t")
		d.Transact(func(tx *Transaction) { txt.Insert(tx, 0, "Hello, 世界 — ✨", nil) })
	})

	// One transaction per keystroke: the source holds one struct per
	// character, which an apply squashes into one. The clone holds the
	// source's structs.
	add("text_keystrokes", one, func(d *Doc) {
		txt := d.GetText("t")
		for range 40 {
			d.Transact(func(tx *Transaction) { txt.Insert(tx, txt.Len(), "x", nil) })
		}
	})

	add("text_formatted", one, func(d *Doc) {
		txt := d.GetText("t")
		d.Transact(func(tx *Transaction) {
			txt.Insert(tx, 0, "the quick brown fox", nil)
			txt.Format(tx, 4, 5, Attributes{"bold": true})
			txt.Format(tx, 10, 5, Attributes{"italic": true, "color": "#f00"})
			txt.InsertEmbed(tx, 3, map[string]any{"image": "a.png"}, Attributes{"alt": "a"})
			txt.Insert(tx, 0, "lead", Attributes{"bold": true})
		})
		d.Transact(func(tx *Transaction) {
			txt.Format(tx, 6, 4, Attributes{"bold": nil})
			txt.Delete(tx, 2, 3)
		})
	})

	add("text_delta", one, func(d *Doc) {
		txt := d.GetText("t")
		d.Transact(func(tx *Transaction) {
			txt.ApplyDelta(tx, []Delta{
				{Insert: "hello ", Attributes: Attributes{"bold": true}},
				{Insert: map[string]any{"embed": 3.5}},
				{Insert: "world"},
			})
		})
		d.Transact(func(tx *Transaction) {
			txt.ApplyDelta(tx, []Delta{
				{Retain: 2},
				{Delete: 3},
				{Retain: 2, Attributes: Attributes{"italic": true}},
			})
		})
	})

	add("map_replaced_keys", one, func(d *Doc) {
		m := d.GetMap("m")
		for round := range 4 {
			d.Transact(func(tx *Transaction) {
				m.Set(tx, "a", round)
				m.Set(tx, "b", fmt.Sprintf("v%d", round))
				m.Set(tx, "", round) // the empty-string key
			})
		}
		d.Transact(func(tx *Transaction) {
			m.Set(tx, "nested", map[string]any{"x": []any{1, 2, 3}})
			m.Delete(tx, "b")
		})
	})

	add("map_nested_types", one, func(d *Doc) {
		m := d.GetMap("m")
		d.Transact(func(tx *Transaction) {
			inner := NewMapPrelim()
			m.Set(tx, "inner", inner)
			inner.Set(tx, "k", "v")
			arr := NewArrayPrelim()
			m.Set(tx, "arr", arr)
			arr.Push(tx, []any{1, "two", true})
			txt := NewTextPrelim()
			m.Set(tx, "txt", txt)
			txt.Insert(tx, 0, "nested", Attributes{"bold": true})
		})
	})

	add("array_mixed", one, func(d *Doc) {
		arr := d.GetArray("a")
		d.Transact(func(tx *Transaction) {
			arr.Push(tx, []any{1, 2.5, "three", true, nil, []any{1, 2}, map[string]any{"k": "v"}})
		})
		d.Transact(func(tx *Transaction) {
			arr.Insert(tx, 2, []any{"inserted"})
			arr.Delete(tx, 0, 2)
		})
		d.Transact(func(tx *Transaction) {
			inner := NewArrayPrelim()
			arr.PushType(tx, inner)
			inner.Push(tx, []any{"deep"})
		})
	})

	add("array_moves", one, func(d *Doc) {
		arr := d.GetArray("a")
		d.Transact(func(tx *Transaction) { arr.Push(tx, []any{"a", "b", "c", "d", "e"}) })
		d.Transact(func(tx *Transaction) { arr.Move(tx, 0, 4) })
		d.Transact(func(tx *Transaction) { arr.Move(tx, 3, 1) })
		d.Transact(func(tx *Transaction) { arr.Delete(tx, 2, 1) })
	})

	add("xml", one, func(d *Doc) {
		frag := d.GetXmlFragment("x")
		d.Transact(func(tx *Transaction) {
			for i := range 5 {
				p := NewYXmlElement("paragraph")
				frag.InsertElement(tx, i, p)
				p.SetAttribute(tx, "id", fmt.Sprintf("p%d", i))
				p.SetAttributeValue(tx, "indent", i)
				txt := NewYXmlText()
				p.InsertText(tx, 0, txt)
				txt.Insert(tx, 0, fmt.Sprintf("paragraph %d text", i), Attributes{"bold": i%2 == 0})
			}
		})
		d.Transact(func(tx *Transaction) { frag.Delete(tx, 1, 1) })
	})

	add("deletes_gc_on", one, buildDeletes)
	add("deletes_gc_off", noGC, buildDeletes)

	add("subdoc", one, func(d *Doc) {
		m := d.GetMap("m")
		kept := New(WithGUID("kept"), WithAutoLoad(true), WithCollectionID("c1"))
		gone := New(WithGUID("gone"))
		d.Transact(func(tx *Transaction) {
			m.Set(tx, "kept", kept)
			m.Set(tx, "gone", gone)
		})
		kept.Transact(func(tx *Transaction) { tx.GetText("inner").Insert(tx, 0, "inner state", nil) })
		d.Transact(func(tx *Transaction) { m.Delete(tx, "gone") })
	})

	cases = append(cases, mergedCloneCases(t)...)
	cases = append(cases, parkedCloneCases(t)...)
	cases = append(cases, randomCloneCases(t, 24)...)
	return cases
}

// buildDeletes writes and deletes across the types whose deletes differ: a
// text run split by a delete, a map key, an array range, and a nested type.
func buildDeletes(d *Doc) {
	txt, arr, m := d.GetText("t"), d.GetArray("a"), d.GetMap("m")
	d.Transact(func(tx *Transaction) {
		txt.Insert(tx, 0, "abcdefghijklmnop", Attributes{"bold": true})
		arr.Push(tx, []any{1, 2, 3, 4, 5})
		m.Set(tx, "k", "v")
		inner := NewMapPrelim()
		m.Set(tx, "inner", inner)
		inner.Set(tx, "deep", 1)
	})
	d.Transact(func(tx *Transaction) {
		txt.Delete(tx, 3, 5)
		arr.Delete(tx, 1, 2)
		m.Delete(tx, "inner")
	})
}

// mergedCloneCases returns the documents two clients editing concurrently and
// syncing leave: both peers' own copies and a third that applied both.
func mergedCloneCases(t *testing.T) []cloneCase {
	t.Helper()
	optsA := []DocOption{WithClientID(11)}
	optsB := []DocOption{WithClientID(22)}
	a := New(optsA...)
	ta := a.GetText("t")
	a.Transact(func(tx *Transaction) { ta.Insert(tx, 0, "shared base", nil) })

	// Both peers start from the same base, so the edits below are concurrent.
	b := New(optsB...)
	require.NoError(t, ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, nil), nil))
	tb := b.GetText("t")

	a.Transact(func(tx *Transaction) {
		ta.Insert(tx, 6, "AAA", Attributes{"bold": true})
		ta.Delete(tx, 0, 2)
	})
	b.Transact(func(tx *Transaction) {
		tb.Insert(tx, 6, "BBB", Attributes{"italic": true})
		tb.Format(tx, 0, 5, Attributes{"underline": true})
	})
	ua, ub := EncodeStateAsUpdateV1(a, nil), EncodeStateAsUpdateV1(b, nil)
	require.NoError(t, ApplyUpdateV1(a, ub, nil))
	require.NoError(t, ApplyUpdateV1(b, ua, nil))
	both := New(WithClientID(33))
	require.NoError(t, ApplyUpdateV1(both, ua, nil))
	require.NoError(t, ApplyUpdateV1(both, ub, nil))

	return []cloneCase{
		{name: "gen/merged_peer_a", doc: a, opts: optsA},
		{name: "gen/merged_peer_b", doc: b, opts: optsB},
		{name: "gen/merged_third", doc: both, opts: []DocOption{WithClientID(33)}},
	}
}

// parkedCloneCases returns documents holding parked structs and parked delete
// ranges: a peer that got the tail of another's log without its head.
func parkedCloneCases(t *testing.T) []cloneCase {
	t.Helper()
	src := New(WithClientID(5))
	txt, m := src.GetText("t"), src.GetMap("m")
	src.Transact(func(tx *Transaction) {
		txt.Insert(tx, 0, "head of the log", nil)
		m.Set(tx, "k", "v")
	})
	head := src.StateVector()
	src.Transact(func(tx *Transaction) {
		txt.Insert(tx, 15, " and its tail", nil)
		txt.Delete(tx, 0, 4) // deletes structs of the head the peer will not have
		m.Set(tx, "k", "w")
	})
	tail := EncodeStateAsUpdateV1(src, head)

	opts := []DocOption{WithClientID(6)}
	parked := New(opts...)
	_ = ApplyUpdateV1(parked, tail, nil)
	stats := parked.PendingStats()
	require.Positive(t, stats.Items, "the fixture must park structs")
	require.Positive(t, stats.DeleteRanges, "the fixture must park delete ranges")

	// The same peer once more, with the same update parked twice.
	parked2 := New(opts...)
	_ = ApplyUpdateV1(parked2, tail, nil)
	_ = ApplyUpdateV1(parked2, tail, nil)

	return []cloneCase{
		{name: "gen/parked", doc: parked, opts: opts},
		{name: "gen/parked_twice", doc: parked2, opts: opts},
	}
}

// randomCloneCases returns n seeded random documents: two clients running a
// random mix of text, map, array and XML operations, syncing at random points,
// so the corpus holds shapes nobody wrote by hand.
func randomCloneCases(t *testing.T, n int) []cloneCase {
	t.Helper()
	cases := make([]cloneCase, 0, n)
	for seed := range n {
		rng := rand.New(rand.NewPCG(uint64(seed), 0x5eed))
		opts := []DocOption{WithClientID(ClientID(100 + seed))}
		if seed%4 == 3 {
			opts = append(opts, WithGC(false))
		}
		a := New(opts...)
		b := New(WithClientID(ClientID(900 + seed)))
		for range 12 {
			randomOps(a, rng)
			randomOps(b, rng)
			if rng.IntN(3) == 0 {
				require.NoError(t, ApplyUpdateV1(a, EncodeStateAsUpdateV1(b, a.StateVector()), nil))
			}
			if rng.IntN(3) == 0 {
				require.NoError(t, ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, b.StateVector()), nil))
			}
		}
		cases = append(cases, cloneCase{name: fmt.Sprintf("gen/random_%d", seed), doc: a, opts: opts})
	}
	return cases
}

// randomOps runs one transaction of random operations on d.
func randomOps(d *Doc, rng *rand.Rand) {
	txt, arr, m, frag := d.GetText("t"), d.GetArray("a"), d.GetMap("m"), d.GetXmlFragment("x")
	d.Transact(func(tx *Transaction) {
		for range 1 + rng.IntN(4) {
			switch rng.IntN(9) {
			case 0:
				txt.Insert(tx, rng.IntN(txt.Len()+1), "lorem ipsum"[:1+rng.IntN(10)], nil)
			case 1:
				txt.Insert(tx, rng.IntN(txt.Len()+1), "bold", Attributes{"bold": true})
			case 2:
				if txt.Len() > 2 {
					at := rng.IntN(txt.Len() - 1)
					txt.Format(tx, at, 1+rng.IntN(txt.Len()-at-1), Attributes{"italic": rng.IntN(2) == 0})
				}
			case 3:
				if txt.Len() > 1 {
					at := rng.IntN(txt.Len() - 1)
					txt.Delete(tx, at, 1+rng.IntN(txt.Len()-at-1))
				}
			case 4:
				txt.InsertEmbed(tx, rng.IntN(txt.Len()+1), map[string]any{"e": rng.IntN(10)}, nil)
			case 5:
				m.Set(tx, fmt.Sprintf("k%d", rng.IntN(5)), rng.IntN(100))
			case 6:
				arr.Push(tx, []any{rng.IntN(100), "s"})
				if arr.Len() > 3 {
					arr.Delete(tx, rng.IntN(arr.Len()-1), 1)
				}
			case 7:
				e := NewYXmlElement("p")
				frag.InsertElement(tx, rng.IntN(frag.Len()+1), e)
				e.SetAttribute(tx, "k", fmt.Sprintf("%d", rng.IntN(10)))
			case 8:
				if frag.Len() > 0 {
					frag.Delete(tx, rng.IntN(frag.Len()), 1)
				}
			}
		}
	})
}
