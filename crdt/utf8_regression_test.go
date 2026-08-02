package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The four shapes measured on the issue: each silently produced an update that
// no decoder would accept, in both V1 and V2. Each must now fail at the call.
func TestInteg_InvalidUTF8_NeverReachesTheWire(t *testing.T) {
	bad := string([]byte{0xff, 0xfe})
	cases := map[string]func(){
		"YText content": func() {
			d := New()
			txt := d.GetText("t")
			d.Transact(func(txn *Transaction) { txt.Insert(txn, 0, bad, nil) })
		},
		"YMap value": func() {
			d := New()
			m := d.GetMap("m")
			d.Transact(func(txn *Transaction) { m.Set(txn, "k", bad) })
		},
		"YMap key": func() {
			d := New()
			m := d.GetMap("m")
			d.Transact(func(txn *Transaction) { m.Set(txn, bad, "v") })
		},
		"root type name": func() { New().GetText(bad) },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) { requirePanicsWithInvalidUTF8(t, build) })
	}
}

// Everything a real document contains must round-trip unchanged in V1 and V2.
func TestInteg_ValidUnicode_RoundTripsUnchanged(t *testing.T) {
	doc := New(WithClientID(4242))
	txt := doc.GetText("t")
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		txt.Insert(txn, 0, "héllo 😀 wörld á", nil)
		m.Set(txn, "🔑", map[string]any{"n": []any{"ok", "ünïcode"}})
	})

	for _, tc := range []struct {
		name  string
		enc   func() []byte
		apply func(*Doc, []byte) error
	}{
		{"V1", doc.EncodeStateAsUpdate, func(d *Doc, u []byte) error { return ApplyUpdateV1(d, u, nil) }},
		{"V2", func() []byte { return EncodeStateAsUpdateV2(doc, nil) }, func(d *Doc, u []byte) error { return ApplyUpdateV2(d, u, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := New()
			require.NoError(t, tc.apply(got, tc.enc()))
			require.Equal(t, txt.ToString(), got.GetText("t").ToString())
		})
	}
}
