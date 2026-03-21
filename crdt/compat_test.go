package crdt_test

import (
	"os"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadFixture reads a binary fixture file from testutil/fixtures/.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../testutil/fixtures/" + name + ".bin")
	require.NoError(t, err, "fixture %s.bin not found — run testutil/gen_fixtures.js", name)
	return data
}

func TestCompat_ApplyJSUpdate_YText(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(doc, loadFixture(t, "ytext_hello"), nil))

	txt := doc.GetText("content")
	assert.Equal(t, "Hello, world!", txt.ToString())
	assert.Equal(t, 13, txt.Len())
}

func TestCompat_ApplyJSUpdate_YTextDelete(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(doc, loadFixture(t, "ytext_delete"), nil))

	txt := doc.GetText("content")
	// "Hello, world!" with delete(5, 7) removes ", world" → "Hello!"
	assert.Equal(t, "Hello!", txt.ToString())
}

func TestCompat_ApplyJSUpdate_YTextBold(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(doc, loadFixture(t, "ytext_bold"), nil))

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

func TestCompat_ApplyJSUpdate_YArray(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(doc, loadFixture(t, "yarray_mixed"), nil))

	arr := doc.GetArray("list")
	got := arr.ToSlice()
	require.Len(t, got, 5)
	assert.Equal(t, int(1), got[0])
	assert.Equal(t, "two", got[1])
	assert.Equal(t, true, got[2])
	assert.Nil(t, got[3])
	assert.Equal(t, map[string]any{"key": "val"}, got[4])
}

func TestCompat_ApplyJSUpdate_YMap(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(doc, loadFixture(t, "ymap_basic"), nil))

	m := doc.GetMap("data")
	name, ok := m.Get("name")
	require.True(t, ok)
	assert.Equal(t, "Alice", name)

	age, ok := m.Get("age")
	require.True(t, ok)
	assert.Equal(t, int(30), age)

	active, ok := m.Get("active")
	require.True(t, ok)
	assert.Equal(t, true, active)
}

func TestCompat_ApplyJSUpdate_ConcurrentMerge(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(doc, loadFixture(t, "merge_concurrent_text"), nil))

	// client 1 ("Alice") and client 2 ("Bob") insert at pos 0 concurrently.
	// YATA: lower clientID wins → "AliceBob".
	txt := doc.GetText("t")
	assert.Equal(t, "AliceBob", txt.ToString())
}

func TestCompat_EmptyDoc(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(99))
	require.NoError(t, crdt.ApplyUpdateV1(doc, loadFixture(t, "empty_doc"), nil))
	// Empty doc: state vector should be empty, no content.
	assert.Equal(t, crdt.StateVector{}, doc.StateVector())
}
