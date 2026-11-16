package crdt_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// anyNumberFixture is one row of testdata/anynumber_yjs_fixtures.json, which
// testutil/gen_fixtures_anynumber.js writes from the pinned yjs: a document
// with clientID 1 pushes one Number into the YArray "a".
type anyNumberFixture struct {
	Name string `json:"name"`
	V1   string `json:"v1"`
	V2   string `json:"v2"`
	JSON string `json:"json"`
}

// anyNumberGoValues lists, for each fixture row, Go integers that hold its
// Number. WriteAny must write each one as Yjs writes the Number.
var anyNumberGoValues = map[string][]any{
	"2^31-1":    {int64(math.MaxInt32), int32(math.MaxInt32), int(math.MaxInt32), uint32(math.MaxInt32)},
	"-(2^31-1)": {int64(-math.MaxInt32), int32(-math.MaxInt32), int(-math.MaxInt32)},
	"2^31":      {int64(1 << 31), uint32(1 << 31), uint64(1 << 31)},
	"-2^31":     {int64(math.MinInt32), int32(math.MinInt32), int(math.MinInt32)},
	"2^31+1":    {int64(1<<31 + 1), uint32(1<<31 + 1), uint64(1<<31 + 1)},
	"-2^31-1":   {int64(-(1 << 31) - 1)},
	"3*2^35":    {int64(3 << 35), uint64(3 << 35)},
	"2^40":      {int64(1 << 40), uint64(1 << 40)},
	"-2^40":     {int64(-(1 << 40))},
	"2^53-1":    {int64(1<<53 - 1), uint64(1<<53 - 1)},
	"2^53":      {int64(1 << 53), uint64(1 << 53)},
	"-2^53":     {int64(-(1 << 53))},
	"2^63":      {uint64(1 << 63)},
	"2^64":      {uint64(math.MaxUint64)}, // 2^64-1, which is 2^64 as a float64
}

func loadAnyNumberFixtures(t *testing.T) []anyNumberFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "anynumber_yjs_fixtures.json"))
	require.NoError(t, err, "run `make fixtures`")
	var fixtures []anyNumberFixture
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.NotEmpty(t, fixtures)
	return fixtures
}

// A Go document with the generator's clientID pushes each Go integer and must
// write the update Yjs wrote for the same Number, in V1 and in V2.
func TestCompat_AnyNumbers_GoWritesYjsBytes(t *testing.T) {
	fixtures := loadAnyNumberFixtures(t)
	require.Len(t, anyNumberGoValues, len(fixtures), "one Go value set per fixture row")
	for _, fx := range fixtures {
		values, ok := anyNumberGoValues[fx.Name]
		require.True(t, ok, "no Go values for fixture %q", fx.Name)
		for _, v := range values {
			t.Run(fmt.Sprintf("%s/%T", fx.Name, v), func(t *testing.T) {
				doc := crdt.New(crdt.WithClientID(1))
				arr := doc.GetArray("a")
				doc.Transact(func(txn *crdt.Transaction) { arr.Push(txn, []any{v}) })
				assert.Equal(t, fx.V1, hex.EncodeToString(crdt.EncodeStateAsUpdateV1(doc, nil)), "V1")
				assert.Equal(t, fx.V2, hex.EncodeToString(crdt.EncodeStateAsUpdateV2(doc, nil)), "V2")
			})
		}
	}
}

// A document that applies the update Yjs wrote prints the array as Yjs does.
func TestCompat_AnyNumbers_ReadYjsBytes(t *testing.T) {
	for _, fx := range loadAnyNumberFixtures(t) {
		for _, version := range []struct {
			name  string
			hexed string
			apply func(*crdt.Doc, []byte, any) error
		}{
			{"V1", fx.V1, crdt.ApplyUpdateV1},
			{"V2", fx.V2, crdt.ApplyUpdateV2},
		} {
			t.Run(fx.Name+"/"+version.name, func(t *testing.T) {
				update, err := hex.DecodeString(version.hexed)
				require.NoError(t, err)
				doc := crdt.New()
				require.NoError(t, version.apply(doc, update, nil))
				js, err := doc.GetArray("a").ToJSON()
				require.NoError(t, err)
				assert.Equal(t, fx.JSON, string(js))
			})
		}
	}
}
