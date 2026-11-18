package crdt

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// binaryAttr returns the "k" format value of doc's text "t".
func binaryAttr(t *testing.T, d *Doc) []byte {
	t.Helper()
	for it := d.GetText("t").start; it != nil; it = it.Right {
		if cf, ok := it.Content.(*ContentFormat); ok && cf.Key == "k" {
			b, ok := cf.Val.([]byte)
			require.True(t, ok, "the attribute decoded as %T, want []byte", cf.Val)
			return b
		}
	}
	t.Fatal("no binary attribute in the document")
	return nil
}

// A caller may reuse or pool the buffer it passed to ApplyUpdateV2: nothing in
// the API says it has to outlive the call. The document must not point into it.
func TestUnit_ApplyUpdateV2_DecodedBytesDoNotAliasTheUpdate(t *testing.T) {
	src := New()
	st := src.GetText("t")
	src.Transact(func(txn *Transaction) {
		st.Insert(txn, 0, "ab", Attributes{"k": []byte{1, 2, 3}})
	})
	update := EncodeStateAsUpdateV2(src, nil)

	dst := New()
	dst.GetText("t")
	require.NoError(t, ApplyUpdateV2(dst, update, nil))

	got := binaryAttr(t, dst)
	want := append([]byte(nil), got...)
	require.Equal(t, []byte{1, 2, 3}, want, "the attribute did not decode")

	for i := range update {
		update[i] ^= 0xFF
	}
	require.True(t, bytes.Equal(want, binaryAttr(t, dst)),
		"the document's binary attribute changed when the caller reused its update buffer")
}

// The same for a binary value nested inside an Any map, which takes the same
// decode path.
func TestUnit_ApplyUpdateV2_NestedDecodedBytesDoNotAliasTheUpdate(t *testing.T) {
	src := New()
	sm := src.GetMap("m")
	src.Transact(func(txn *Transaction) {
		sm.Set(txn, "k", map[string]any{"b": []byte{9, 8, 7}})
	})
	update := EncodeStateAsUpdateV2(src, nil)

	dst := New()
	dst.GetMap("m")
	require.NoError(t, ApplyUpdateV2(dst, update, nil))

	read := func() []byte {
		v, ok := dst.GetMap("m").Get("k")
		require.True(t, ok)
		m, ok := v.(map[string]any)
		require.True(t, ok, "value decoded as %T", v)
		b, ok := m["b"].([]byte)
		require.True(t, ok, "nested value decoded as %T", m["b"])
		return b
	}
	want := append([]byte(nil), read()...)
	require.Equal(t, []byte{9, 8, 7}, want)

	for i := range update {
		update[i] ^= 0xFF
	}
	require.True(t, bytes.Equal(want, read()),
		"a nested binary value changed when the caller reused its update buffer")
}
