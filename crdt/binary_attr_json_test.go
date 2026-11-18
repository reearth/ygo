package crdt

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Yjs writes a format or embed value with JSON.stringify, and a Uint8Array
// stringifies as an object keyed by index: JSON.stringify(new Uint8Array([1,2]))
// is {"0":1,"1":2}. encoding/json writes base64 for a Go []byte, so a document
// ygo wrote carried a value no Yjs peer could read back.
func TestUnit_FmtValToJSON_WritesBinaryAsYjsDoes(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"empty", []byte{}, `{}`},
		{"one byte", []byte{1}, `{"0":1}`},
		{"two bytes", []byte{1, 2}, `{"0":1,"1":2}`},
		{"high byte", []byte{255}, `{"0":255}`},
		{"nested in a map", map[string]any{"k": []byte{1, 2}}, `{"k":{"0":1,"1":2}}`},
		{"nested in a slice", []any{[]byte{3}}, `[{"0":3}]`},
		{"deeply nested", map[string]any{"a": []any{map[string]any{"b": []byte{7}}}}, `{"a":[{"b":{"0":7}}]}`},

		// Unchanged for everything else.
		{"string", "x", `"x"`},
		{"number", float64(1), `1`},
		{"bool", true, `true`},
		{"nil", nil, `null`},
		{"plain map", map[string]any{"k": 1}, `{"k":1}`},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, fmtValToJSON(c.val), c.name)
	}
}

// End to end: the V1 update must carry the Yjs form, not base64.
func TestUnit_EncodeStateAsUpdateV1_BinaryAttributeIsNotBase64(t *testing.T) {
	d := New()
	txt := d.GetText("t")
	d.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "ab", Attributes{"k": []byte{1, 2}})
	})
	update := EncodeStateAsUpdateV1(d, nil)

	assert.NotContains(t, string(update), "AQI=", "the V1 update still carries base64")
	assert.True(t, bytes.Contains(update, []byte(`{"0":1,"1":2}`)),
		"the V1 update does not carry the Yjs form of the binary attribute")
}

// A replica reads what Yjs's peers read: a plain object, not a Go []byte. V1
// format values are JSON, which has no binary type, so this is lossy for Yjs
// too. V2 carries the type and is unaffected.
func TestUnit_BinaryAttribute_V1IsLossyAndV2IsNot(t *testing.T) {
	src := New()
	st := src.GetText("t")
	src.Transact(func(txn *Transaction) {
		st.Insert(txn, 0, "ab", Attributes{"k": []byte{1, 2}})
	})

	read := func(d *Doc) any {
		for it := d.GetText("t").start; it != nil; it = it.Right {
			if cf, ok := it.Content.(*ContentFormat); ok && cf.Key == "k" {
				return cf.Val
			}
		}
		return nil
	}

	v1 := New()
	v1.GetText("t")
	require.NoError(t, ApplyUpdateV1(v1, EncodeStateAsUpdateV1(src, nil), nil))
	assert.Equal(t, map[string]any{"0": float64(1), "1": float64(2)}, read(v1),
		"a V1 replica reads the index-keyed object, as a Yjs peer does")

	v2 := New()
	v2.GetText("t")
	require.NoError(t, ApplyUpdateV2(v2, EncodeStateAsUpdateV2(src, nil), nil))
	assert.Equal(t, []byte{1, 2}, read(v2), "V2 carries the binary type")
}

// Byte-for-byte against yjs 13.6.30. The reference is the update
// Y.encodeStateAsUpdate produces for the same document:
//
//	const d = new Y.Doc({ guid: 'g' }); d.clientID = 7
//	d.getText('t').insert(0, 'ab', { k: new Uint8Array([1, 2]) })
func TestCompat_BinaryAttribute_V1BytesMatchYjs(t *testing.T) {
	want := []byte{
		1, 3, 7, 0, 6, 1, 1, 116, 1, 107, 13,
		123, 34, 48, 34, 58, 49, 44, 34, 49, 34, 58, 50, 125, // {"0":1,"1":2}
		132, 7, 0, 2, 97, 98,
		134, 7, 2, 1, 107, 4, 110, 117, 108, 108,
		0,
	}

	d := newTestDoc(7)
	txt := d.GetText("t")
	d.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "ab", Attributes{"k": []byte{1, 2}})
	})
	assert.Equal(t, want, EncodeStateAsUpdateV1(d, nil))
}
