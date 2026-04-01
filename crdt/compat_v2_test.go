package crdt_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

func loadFixtureV2(t *testing.T, name string) []byte {
	t.Helper()
	return loadFixture(t, name+"_v2")
}

func TestCompatV2_ApplyJSUpdate_YText(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV2(doc, loadFixtureV2(t, "ytext_hello"), nil))

	txt := doc.GetText("content")
	assert.Equal(t, "Hello, world!", txt.ToString())
	assert.Equal(t, 13, txt.Len())
}

func TestCompatV2_ApplyJSUpdate_YTextDelete(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV2(doc, loadFixtureV2(t, "ytext_delete"), nil))

	txt := doc.GetText("content")
	assert.Equal(t, "Hello!", txt.ToString())
}

func TestCompatV2_ApplyJSUpdate_YTextBold(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV2(doc, loadFixtureV2(t, "ytext_bold"), nil))

	txt := doc.GetText("content")
	assert.Equal(t, "Hello, world!", txt.ToString())

	delta := txt.ToDelta()
	require.Len(t, delta, 2)
	assert.Equal(t, crdt.DeltaOpInsert, delta[0].Op)
	assert.Equal(t, "Hello", delta[0].Insert)
	assert.Equal(t, crdt.Attributes{"bold": true}, delta[0].Attributes)
	assert.Equal(t, crdt.DeltaOpInsert, delta[1].Op)
	assert.Equal(t, ", world!", delta[1].Insert)
	assert.Nil(t, delta[1].Attributes)
}

func TestCompatV2_ApplyJSUpdate_YArray(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV2(doc, loadFixtureV2(t, "yarray_mixed"), nil))

	arr := doc.GetArray("list")
	got := arr.ToSlice()
	require.Len(t, got, 5)
	assert.Equal(t, int64(1), got[0])
	assert.Equal(t, "two", got[1])
	assert.Equal(t, true, got[2])
	assert.Nil(t, got[3])
	assert.Equal(t, map[string]any{"key": "val"}, got[4])
}

func TestCompatV2_ApplyJSUpdate_YMap(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV2(doc, loadFixtureV2(t, "ymap_basic"), nil))

	m := doc.GetMap("data")
	name, ok := m.Get("name")
	require.True(t, ok)
	assert.Equal(t, "Alice", name)

	age, ok := m.Get("age")
	require.True(t, ok)
	assert.Equal(t, int64(30), age)

	active, ok := m.Get("active")
	require.True(t, ok)
	assert.Equal(t, true, active)
}

func TestCompatV2_ApplyJSUpdate_ConcurrentMerge(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV2(doc, loadFixtureV2(t, "merge_concurrent_text"), nil))

	txt := doc.GetText("t")
	assert.Equal(t, "AliceBob", txt.ToString())
}

func TestCompatV2_EmptyDoc(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV2(doc, loadFixtureV2(t, "empty_doc"), nil))
	assert.Equal(t, crdt.StateVector{}, doc.StateVector())
}

// TestCompatV2_RoundTrip encodes with Go V2 and decodes, verifying content survives.
func TestCompatV2_RoundTrip(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "Hello V2!", nil)
	})

	v2 := crdt.EncodeStateAsUpdateV2(doc, nil)

	doc2 := crdt.New(crdt.WithClientID(2))
	require.NoError(t, crdt.ApplyUpdateV2(doc2, v2, nil))
	assert.Equal(t, "Hello V2!", doc2.GetText("content").ToString())
}

// TestCompatV2_V1toV2toV1 verifies the conversion helpers round-trip correctly.
func TestCompatV2_V1toV2toV1(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "roundtrip", nil)
	})

	v1 := crdt.EncodeStateAsUpdateV1(doc, nil)
	v2, err := crdt.UpdateV1ToV2(v1)
	require.NoError(t, err)

	v1Back, err := crdt.UpdateV2ToV1(v2)
	require.NoError(t, err)

	doc2 := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(doc2, v1Back, nil))
	assert.Equal(t, "roundtrip", doc2.GetText("t").ToString())
}
