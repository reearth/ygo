package crdt

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type mergeStructsFixtures struct {
	Sessions map[string][][]any `json:"sessions"`
	Rows     []mergeStructsRow  `json:"rows"`
}

type mergeStructsRow struct {
	Name    string   `json:"name"`
	GC      bool     `json:"gc"`
	Session string   `json:"session"`
	Updates []string `json:"updates"`
	V1      string   `json:"v1"`
	V2      string   `json:"v2"`
}

// mergeStructsReplay applies ops to a fresh document of client 1, one
// transaction per op, as testutil/gen_fixtures_mergestructs.js does in yjs.
func mergeStructsReplay(t *testing.T, gc bool, ops [][]any) *Doc {
	t.Helper()
	doc := New(WithClientID(1), WithGC(gc))
	text, arr, m := doc.GetText("t"), doc.GetArray("a"), doc.GetMap("m")
	for _, op := range ops {
		n := func(i int) int { return int(op[i].(float64)) }
		doc.Transact(func(txn *Transaction) {
			switch op[0] {
			case "ins":
				text.Insert(txn, n(1), op[2].(string), nil)
			case "del":
				text.Delete(txn, n(1), n(2))
			case "del2":
				text.Delete(txn, n(2), 1)
				text.Delete(txn, n(1), 1)
			case "mset":
				m.Set(txn, op[1].(string), n(2))
			case "ains":
				arr.Insert(txn, n(1), []any{n(2)})
			case "adel":
				arr.Delete(txn, n(1), n(2))
			case "ains3":
				for k := range 3 {
					arr.Insert(txn, n(1)+k, []any{n(2) + k})
				}
			case "ainsm":
				for _, pv := range op[1].([]any) {
					pv := pv.([]any)
					arr.Insert(txn, int(pv[0].(float64)), []any{int(pv[1].(float64))})
				}
			default:
				t.Fatalf("unknown op %v", op)
			}
		})
	}
	return doc
}

// assertSameBytes compares an encoding with yjs's without dumping either.
func assertSameBytes(t *testing.T, what string, got []byte, yjsHex string) {
	t.Helper()
	want := mustHex(t, yjsHex)
	if bytes.Equal(got, want) {
		return
	}
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	t.Errorf("%s: %d bytes, yjs %d bytes, first difference at byte %d", what, len(got), len(want), at)
}

// TestCompat_MergeStructs_StateMatchesYjs replays yjs-authored sessions and
// asserts the full state is byte-identical to yjs's, in V1 and V2. The bytes
// match only if ygo merges structs at the end of each transaction as yjs
// does: deleted structs with their neighbours, and added structs with the
// struct on their left, also one from an earlier transaction.
func TestCompat_MergeStructs_StateMatchesYjs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "mergestructs_yjs_fixtures.json"))
	require.NoError(t, err)
	var fx mergeStructsFixtures
	require.NoError(t, json.Unmarshal(raw, &fx))
	require.NotEmpty(t, fx.Rows)
	for _, row := range fx.Rows {
		t.Run(row.Name, func(t *testing.T) {
			var doc *Doc
			if row.Updates == nil {
				ops, ok := fx.Sessions[row.Session]
				require.True(t, ok, "unknown session %q", row.Session)
				doc = mergeStructsReplay(t, row.GC, ops)
			} else {
				doc = New(WithClientID(2), WithGC(row.GC))
				for _, u := range row.Updates {
					require.NoError(t, ApplyUpdateV1(doc, mustHex(t, u), nil))
				}
			}
			assertSameBytes(t, "V1 state", EncodeStateAsUpdateV1(doc, nil), row.V1)
			assertSameBytes(t, "V2 state", EncodeStateAsUpdateV2(doc, nil), row.V2)
		})
	}
}
